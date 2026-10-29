package gitpolicy

import (
	"fmt"
	"slices"
	"strings"
)

// gitRun is one git invocation being classified.
type gitRun struct {
	c    *classifier
	dir  string // where it runs, after -C
	root string // the Worktree or Writable folder holding dir, "" if none
	sub  string
	args []string
}

// git classifies `git args...`; env holds the variables set for it.
func (c *classifier) git(cmd *command, words []word, env []string) Decision {
	if cmd.subst {
		return user("uses command substitution with git")
	}
	for _, name := range env {
		if !safeEnv(name) {
			return bypass("sets " + name + " for git")
		}
	}
	if c.taint != "" {
		return user(c.taint + " before running git")
	}
	if !c.cwdKnown {
		return user("can't tell which folder git runs in")
	}
	globs := false
	for _, w := range words {
		if w.dynamic {
			return user("uses shell variables or expansions in a git command")
		}
		globs = globs || w.glob && c.sh == Bash
	}

	g := &gitRun{c: c, dir: c.cwd}
	i := 0
	for ; i < len(words); i++ {
		a := words[i].text
		if !strings.HasPrefix(a, "-") {
			break
		}
		opt, _, hasValue := strings.Cut(a, "=")
		switch opt {
		case "-C":
			if i+1 >= len(words) {
				return user("can't parse the git command")
			}
			i++
			g.dir = resolve(g.dir, words[i].text)
		case "-c", "--config-env":
			return bypass("git " + opt + " changes git's config for one command")
		case "--git-dir", "--work-tree", "--namespace", "--exec-path", "--super-prefix", "--bare", "--list-cmds", "--attr-source":
			return bypass("git " + opt + " points git somewhere else")
		case "-p", "--paginate", "-P", "--no-pager", "--no-replace-objects", "--literal-pathspecs", "--glob-pathspecs",
			"--noglob-pathspecs", "--icase-pathspecs", "--no-optional-locks", "--no-lazy-fetch", "--no-advice":
			if hasValue {
				return user("unknown git option " + a)
			}
		case "--version", "-v", "--help", "-h", "--html-path", "--man-path", "--info-path":
			return Decision{Verdict: Allow, Reason: "read-only git"}
		default:
			return user("unknown git option " + a)
		}
	}
	if i >= len(words) {
		return Decision{Verdict: Allow, Reason: "read-only git"}
	}
	g.sub = words[i].text
	for _, w := range words[i+1:] {
		g.args = append(g.args, w.text)
	}

	readable := c.writable(g.dir) || insideAny(g.dir, c.sc.Readable)
	if !readable {
		return user("runs git outside the folders you may use")
	}
	g.root = c.rootOf(g.dir)

	rule, ok := gitRules[g.sub]
	if !ok {
		if _, alias := c.lk.Alias(g.sub); alias {
			return user("git alias")
		}
		return user(fmt.Sprintf("unknown git command %q", g.sub))
	}
	if globs && !pathspecCommands[g.sub] {
		return user("unquoted wildcard in a git command")
	}
	for _, a := range g.args {
		if a == "--" {
			break
		}
		if len(a) > 7 && strings.HasPrefix("--no-verify", a) {
			// Skips hooks: the user's own, and the guard's pre-push.
			return bypass("git " + g.sub + " --no-verify skips hooks")
		}
	}
	return rule(g)
}

func (c *classifier) writable(dir string) bool {
	return inside(dir, c.sc.Worktree) || insideAny(dir, c.sc.Writable)
}

// rootOf returns the Worktree or Writable folder holding dir.
func (c *classifier) rootOf(dir string) string {
	if inside(dir, c.sc.Worktree) {
		return c.sc.Worktree
	}
	for _, w := range c.sc.Writable {
		if inside(dir, w) {
			return w
		}
	}
	return ""
}

// pathspecCommands take file names, where Bash wildcards are routine.
var pathspecCommands = map[string]bool{
	"add": true, "rm": true, "mv": true, "diff": true, "log": true, "show": true, "status": true,
	"ls-files": true, "ls-tree": true, "grep": true, "blame": true, "restore": true, "checkout": true,
	"commit": true, "stash": true, "clean": true, "check-ignore": true, "check-attr": true,
	"shortlog": true, "whatchanged": true, "apply": true, "am": true, "annotate": true,
}

// --- decisions ---

func allowRead() Decision { return Decision{Verdict: Allow, Reason: "read-only git"} }

func protectedRef(verb, ref string) Decision {
	return Decision{Verdict: User, Reason: verb + " protected branch " + shortBranch(ref), Protected: true, Refs: []string{shortBranch(ref)}}
}

func destructive(reason string, refs ...string) Decision {
	return Decision{Verdict: User, Reason: reason, Destructive: true, Refs: refs}
}

// write allows a routine write: it must run in the agent's own folders, and
// the git guard must be active there.
func (g *gitRun) write(reason string, refs ...string) Decision {
	if g.root == "" {
		return Decision{Verdict: User, Reason: "changes a repository outside your worktree", Bypass: true, Refs: refs}
	}
	if !g.c.sc.Guarded {
		return Decision{Verdict: User, Reason: "git guard not active in this worktree", Bypass: true, Refs: refs}
	}
	return Decision{Verdict: Allow, Reason: reason, Refs: refs}
}

func (g *gitRun) protected(ref string) bool {
	return isProtected(ref, g.c.sc.Protected)
}

// current returns the branch checked out where g runs, "" when detached.
func (g *gitRun) current() (string, error) {
	if g.root != "" {
		if b, ok := g.c.moved[g.root]; ok {
			return b, nil
		}
	}
	b, err := g.c.lk.CurrentBranch(g.dir)
	return shortBranch(b), err
}

// moveTo records that the worktree switched to branch ("" detached), so git
// later in the same line sees it.
func (g *gitRun) moveTo(branch string) {
	if g.root != "" {
		g.c.moved[g.root] = shortBranch(branch)
	}
}

// onOwnBranch allows a write that changes the checked-out branch, unless that
// branch is protected or can't be determined.
func (g *gitRun) onOwnBranch(verb, reason string) Decision {
	b, err := g.current()
	if err != nil {
		return user("can't tell which branch is checked out")
	}
	if b != "" && g.protected(b) {
		return protectedRef(verb, b)
	}
	if b == "" {
		return g.write(reason)
	}
	return g.write(reason, b)
}

func (g *gitRun) branchExists(name string) bool {
	return g.c.lk.RefExists("refs/heads/" + shortBranch(name))
}

// --- options ---

// optSpec says which options take a value, so positional arguments can be
// told apart from option values. Unknown options are taken as flags.
type optSpec struct {
	short string   // short options that take a value
	long  []string // long options that take a value
	flags []string // long flags, for resolving abbreviations
}

type opts struct {
	set   map[string]bool     // "-x" and "--name", after abbreviations
	vals  map[string][]string // values of value-taking options
	pos   []string            // positional arguments before --
	paths []string            // arguments after --
	dash  bool                // -- was given
}

func (o opts) has(names ...string) bool {
	for _, n := range names {
		if o.set[n] {
			return true
		}
	}
	return false
}

func (o opts) val(names ...string) (string, bool) {
	for _, n := range names {
		if v := o.vals[n]; len(v) > 0 {
			return v[len(v)-1], true
		}
	}
	return "", false
}

// hasPrefix reports whether an option starting with prefix was given, e.g.
// --force-with-lease=x for "--force".
func (o opts) hasPrefix(prefix string) bool {
	for n := range o.set {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// longName resolves an abbreviated long option against the known names.
func (s optSpec) longName(name string) string {
	if slices.Contains(s.long, name) || slices.Contains(s.flags, name) {
		return name
	}
	match := ""
	for _, n := range append(slices.Clone(s.long), s.flags...) {
		if strings.HasPrefix(n, name) {
			if match != "" {
				return name // ambiguous: git refuses it
			}
			match = n
		}
	}
	if match == "" {
		return name
	}
	return match
}

func parseOpts(args []string, s optSpec) opts {
	o := opts{set: map[string]bool{}, vals: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case o.dash:
			o.paths = append(o.paths, a)
		case a == "--":
			o.dash = true
		case strings.HasPrefix(a, "--"):
			name, value, hasValue := strings.Cut(a[2:], "=")
			name = s.longName(name)
			o.set["--"+name] = true
			if hasValue {
				o.vals["--"+name] = append(o.vals["--"+name], value)
			} else if slices.Contains(s.long, name) && i+1 < len(args) {
				i++
				o.vals["--"+name] = append(o.vals["--"+name], args[i])
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			letters := []rune(a[1:])
			for j, l := range letters {
				o.set["-"+string(l)] = true
				if strings.ContainsRune(s.short, l) {
					if j+1 < len(letters) {
						o.vals["-"+string(l)] = append(o.vals["-"+string(l)], string(letters[j+1:]))
					} else if i+1 < len(args) {
						i++
						o.vals["-"+string(l)] = append(o.vals["-"+string(l)], args[i])
					}
					break
				}
			}
		default:
			o.pos = append(o.pos, a)
		}
	}
	return o
}

// --- rules ---

var gitRules map[string]func(*gitRun) Decision

func init() {
	gitRules = map[string]func(*gitRun) Decision{
		"add": ruleAdd, "rm": ruleRmMv, "mv": ruleRmMv, "commit": ruleCommit, "switch": ruleSwitch,
		"checkout": ruleCheckout, "branch": ruleBranch, "merge": ruleMerge, "rebase": ruleRebase,
		"cherry-pick": ruleReplay, "revert": ruleReplay, "am": ruleReplay, "stash": ruleStash,
		"fetch": ruleFetch, "pull": rulePull, "push": rulePush, "reset": ruleReset, "restore": ruleRestore,
		"clean": ruleClean, "tag": ruleTag, "remote": ruleRemote, "worktree": ruleWorktree,
		"config": ruleConfig, "reflog": ruleReflog, "symbolic-ref": ruleSymbolicRef, "update-ref": ruleUpdateRef,
		"apply": ruleApply, "format-patch": ruleFormatPatch, "archive": ruleArchive, "notes": ruleListOnly("list", "show"),
		"submodule": ruleListOnly("status", "summary"), "sparse-checkout": ruleListOnly("list"),
		"bundle": ruleListOnly("verify", "list-heads"), "rerere": ruleListOnly("status", "diff", "remaining"),
		"merge-file": ruleMergeFile, "hash-object": ruleHashObject, "fsck": ruleFsck,
		"gc": ruleGC, "prune": destructiveRule("deletes unreachable objects"),
		"filter-branch": destructiveRule("rewrites history with filter-branch"),
		"filter-repo":   destructiveRule("rewrites history with filter-repo"),
		"replace":       destructiveRule("replaces objects with git replace"),
	}
	for _, sub := range []string{
		"status", "diff", "log", "show", "blame", "annotate", "rev-parse", "ls-files", "ls-tree", "grep",
		"describe", "shortlog", "merge-base", "cat-file", "rev-list", "show-ref", "for-each-ref", "name-rev",
		"whatchanged", "show-branch", "cherry", "range-diff", "diff-tree", "diff-files", "diff-index",
		"count-objects", "check-ignore", "check-attr", "check-ref-format", "check-mailmap", "var", "version",
		"help", "verify-commit", "verify-tag", "ls-remote", "merge-tree", "get-tar-commit-id", "column",
		"request-pull", "verify-pack", "show-index",
	} {
		gitRules[sub] = ruleRead
	}
}

func destructiveRule(reason string) func(*gitRun) Decision {
	return func(*gitRun) Decision { return destructive(reason) }
}

func ruleRead(g *gitRun) Decision {
	for _, a := range g.args {
		opt, _, _ := strings.Cut(a, "=")
		switch {
		case opt == "--output" || opt == "--output-directory":
			return user("git " + g.sub + " writes a file")
		case g.sub == "grep" && (opt == "-O" || strings.HasPrefix(a, "-O") || strings.HasPrefix("--open-files-in-pager", opt) && len(opt) > 3):
			return user("git grep opens a program")
		case g.sub == "ls-remote" && strings.HasPrefix(opt, "--upload-pack"):
			return user("--upload-pack runs a program")
		}
	}
	return allowRead()
}

func ruleListOnly(read ...string) func(*gitRun) Decision {
	return func(g *gitRun) Decision {
		o := parseOpts(g.args, optSpec{})
		if len(o.pos) > 0 && slices.Contains(read, o.pos[0]) || len(o.pos) == 0 && slices.Contains(read, "list") {
			return allowRead()
		}
		return user("git " + g.sub + " " + strings.Join(o.pos[:min(1, len(o.pos))], "") + " isn't routine")
	}
}

func ruleAdd(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{long: []string{"chmod", "pathspec-from-file"}, flags: []string{"dry-run", "patch", "interactive", "edit", "force", "update", "all"}})
	if o.has("-n", "--dry-run") {
		return allowRead()
	}
	if o.has("-p", "--patch", "-i", "--interactive", "-e", "--edit") {
		return user("interactive git add")
	}
	return g.onOwnBranch("stages files on", "stages files")
}

func ruleRmMv(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{long: []string{"pathspec-from-file"}, flags: []string{"dry-run", "force", "cached"}})
	if o.has("-n", "--dry-run") {
		return allowRead()
	}
	if o.has("-f", "--force") {
		return destructive("git " + g.sub + " --force can lose changes")
	}
	return g.onOwnBranch("changes files on", "git "+g.sub)
}

func ruleCommit(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{
		short: "mFCct",
		long:  []string{"message", "file", "reuse-message", "reedit-message", "fixup", "squash", "author", "date", "template", "cleanup", "trailer", "pathspec-from-file"},
		flags: []string{"amend", "patch", "interactive", "all", "no-verify", "dry-run", "no-edit", "edit"},
	})
	if o.has("--dry-run") {
		return allowRead()
	}
	if o.has("-n") {
		return bypass("git commit -n skips hooks")
	}
	if o.has("-p", "--patch", "--interactive") {
		return user("interactive git commit")
	}
	if o.has("--amend") {
		b, err := g.current()
		if err != nil {
			return user("can't tell which branch is checked out")
		}
		if b != "" && !g.protected(b) && g.c.lk.Pushed(b) {
			return Decision{Verdict: User, Reason: "amends a commit already pushed on " + b, Refs: []string{b}}
		}
	}
	return g.onOwnBranch("commits on", "commits")
}

func ruleSwitch(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{
		short: "cC",
		long:  []string{"create", "force-create", "orphan", "conflict"},
		flags: []string{"detach", "force", "discard-changes", "merge", "track", "no-track", "guess", "no-guess"},
	})
	if o.has("-f", "--force", "--discard-changes") {
		return destructive("git switch --discard-changes drops your changes")
	}
	if b, ok := o.val("-c", "--create", "-C", "--force-create", "--orphan"); ok {
		return g.newBranch(b, o.has("-C", "--force-create"))
	}
	if o.has("-d", "--detach") {
		g.moveTo("")
		return g.write("detaches HEAD")
	}
	if len(o.pos) == 0 {
		return user("can't tell which branch git switch goes to")
	}
	return g.switchTo(o.pos[0])
}

// newBranch handles -b/-c (create) and -B/-C (create or reset).
func (g *gitRun) newBranch(b string, reset bool) Decision {
	if g.protected(b) {
		return protectedRef("switches to", b)
	}
	if reset && g.branchExists(b) {
		return destructive("resets existing branch "+shortBranch(b), shortBranch(b))
	}
	g.moveTo(b)
	return g.write("creates branch "+shortBranch(b), shortBranch(b))
}

// switchTo handles switching the worktree to an existing branch.
func (g *gitRun) switchTo(b string) Decision {
	if b == "-" || strings.HasPrefix(b, "@{") {
		return user("can't tell which branch " + b + " is")
	}
	if g.protected(b) {
		return protectedRef("switches to", b)
	}
	g.moveTo(b)
	return g.write("switches to " + shortBranch(b))
}

func ruleCheckout(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{
		short: "bB",
		long:  []string{"orphan", "conflict", "pathspec-from-file"},
		flags: []string{"force", "patch", "detach", "merge", "ours", "theirs", "track", "no-track", "guess", "no-guess", "overlay", "no-overlay"},
	})
	discard := destructive("git checkout of files discards their changes")
	if o.has("-p", "--patch", "--pathspec-from-file") || len(o.paths) > 0 {
		return discard
	}
	if o.has("-f", "--force", "--ours", "--theirs") {
		return destructive("git checkout --force discards changes")
	}
	if b, ok := o.val("-b", "-B", "--orphan"); ok {
		if len(o.pos) > 1 {
			return discard
		}
		return g.newBranch(b, o.has("-B"))
	}
	switch len(o.pos) {
	case 0:
		if o.has("--detach") {
			g.moveTo("")
			return g.write("detaches HEAD")
		}
		if o.dash {
			return discard
		}
		return allowRead()
	case 1:
	default:
		return discard
	}
	p := o.pos[0]
	if p == "-" || strings.HasPrefix(p, "@{") {
		return user("can't tell which branch " + p + " is")
	}
	if o.has("--detach") {
		g.moveTo("")
		return g.write("detaches HEAD")
	}
	if g.protected(p) {
		return protectedRef("switches to", p)
	}
	if g.branchExists(p) {
		return g.switchTo(shortBranch(p))
	}
	if g.c.lk.RefExists(p) {
		g.moveTo("")
		return g.write("checks out " + p + " (detached)")
	}
	if g.c.lk.RefExists("refs/remotes/origin/" + p) {
		// git creates a local branch tracking origin/<p>.
		if g.protected(p) {
			return protectedRef("switches to", p)
		}
		g.moveTo(p)
		return g.write("creates branch "+p, p)
	}
	return discard
}

func ruleBranch(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{
		short: "u",
		long:  []string{"set-upstream-to", "contains", "no-contains", "merged", "no-merged", "points-at", "sort", "format"},
		flags: []string{"delete", "force", "move", "copy", "list", "all", "remotes", "verbose", "show-current", "unset-upstream", "edit-description", "ignore-case", "track", "no-track", "create-reflog", "quiet", "column", "no-column", "color", "no-color"},
	})
	pos := o.pos
	if o.has("--edit-description") {
		return user("git branch --edit-description opens an editor")
	}
	switch {
	case o.has("-d", "-D", "--delete"):
		if len(pos) == 0 {
			return user("can't parse git branch --delete")
		}
		for _, b := range pos {
			if g.protected(b) {
				d := protectedRef("deletes", b)
				d.Destructive = true
				return d
			}
		}
		if o.has("-D") || o.has("-f", "--force") {
			return destructive("force-deletes branch "+strings.Join(pos, ", "), pos...)
		}
		if o.has("-r", "--remotes") {
			return destructive("deletes remote-tracking branch " + strings.Join(pos, ", "))
		}
		return g.write("deletes merged branch "+strings.Join(pos, ", "), pos...)
	case o.has("-m", "-M", "--move"):
		cur, err := g.current()
		if err != nil {
			return user("can't tell which branch is checked out")
		}
		old, nw := cur, ""
		switch len(pos) {
		case 1:
			nw = pos[0]
		case 2:
			old, nw = pos[0], pos[1]
		default:
			return user("can't parse git branch --move")
		}
		old, nw = shortBranch(old), shortBranch(nw)
		if g.protected(old) {
			return protectedRef("renames", old)
		}
		if g.protected(nw) {
			return protectedRef("renames a branch to", nw)
		}
		if old != cur || old == "" {
			return Decision{Verdict: User, Reason: "renames branch " + old + ", which isn't checked out here", Refs: []string{old, nw}}
		}
		if g.c.lk.Pushed(old) {
			return Decision{Verdict: User, Reason: "renames pushed branch " + old, Refs: []string{old, nw}}
		}
		if (o.has("-M") || o.has("-f", "--force")) && g.branchExists(nw) {
			return destructive("overwrites branch "+nw, old, nw)
		}
		g.moveTo(nw)
		return g.write("renames "+old+" to "+nw, old, nw)
	case o.has("-c", "-C", "--copy"):
		if len(pos) == 0 || len(pos) > 2 {
			return user("can't parse git branch --copy")
		}
		nw := shortBranch(pos[len(pos)-1])
		if g.protected(nw) {
			return protectedRef("copies a branch to", nw)
		}
		if (o.has("-C") || o.has("-f", "--force")) && g.branchExists(nw) {
			return destructive("overwrites branch "+nw, nw)
		}
		return g.write("copies a branch to "+nw, nw)
	case o.has("-u", "--set-upstream-to", "--unset-upstream"):
		b := ""
		if len(pos) > 0 {
			b = pos[0]
		} else {
			cur, err := g.current()
			if err != nil || cur == "" {
				return user("can't tell which branch is checked out")
			}
			b = cur
		}
		if g.protected(b) {
			return protectedRef("changes the upstream of", b)
		}
		return g.write("sets the upstream of "+shortBranch(b), shortBranch(b))
	}
	listing := o.has("-l", "--list", "-a", "--all", "-r", "--remotes", "-v", "-vv", "--verbose", "--show-current",
		"--contains", "--no-contains", "--merged", "--no-merged", "--points-at", "--format", "--sort")
	if len(pos) == 0 || listing {
		if o.has("-f", "--force") && len(pos) > 0 && !o.has("-l", "--list") {
			return user("can't parse git branch")
		}
		return allowRead()
	}
	if len(pos) > 2 {
		return user("can't parse git branch")
	}
	nw := shortBranch(pos[0])
	if g.protected(nw) {
		d := protectedRef("moves", nw)
		d.Destructive = o.has("-f", "--force")
		return d
	}
	if o.has("-f", "--force") && g.branchExists(nw) {
		return destructive("moves existing branch "+nw, nw)
	}
	return g.write("creates branch "+nw, nw)
}

func ruleMerge(g *gitRun) Decision {
	return g.onOwnBranch("merges into", "merges into the current branch")
}

func ruleRebase(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{
		short: "sXxC",
		long:  []string{"onto", "strategy", "strategy-option", "exec", "whitespace", "empty"},
		flags: []string{"continue", "skip", "abort", "quit", "edit-todo", "show-current-patch", "interactive", "exec", "update-refs", "no-update-refs", "root", "autosquash", "rebase-merges", "keep-base", "autostash", "fork-point"},
	})
	switch {
	case o.has("--show-current-patch"):
		return allowRead()
	case o.has("--continue", "--skip", "--abort", "--quit"):
		return g.onOwnBranch("rebases", "continues the rebase")
	case o.has("--edit-todo", "-i", "--interactive"):
		return user("interactive rebase")
	case o.has("-x", "--exec"):
		return user("git rebase --exec runs commands")
	case o.has("--update-refs"):
		return user("git rebase --update-refs moves other branches")
	}
	var target string
	if len(o.pos) >= 2 {
		target = shortBranch(o.pos[1])
		if g.protected(target) {
			return protectedRef("rebases", target)
		}
		if !g.branchExists(target) {
			return user("can't tell which branch git rebase changes")
		}
		g.moveTo(target)
	} else {
		cur, err := g.current()
		if err != nil {
			return user("can't tell which branch is checked out")
		}
		if cur == "" {
			return g.write("rebases a detached HEAD")
		}
		target = cur
		if g.protected(target) {
			return protectedRef("rebases", target)
		}
	}
	if g.c.lk.Pushed(target) {
		return Decision{Verdict: User, Reason: "rebases pushed branch " + target, Destructive: true, Refs: []string{target}}
	}
	return g.write("rebases "+target, target)
}

// ruleReplay covers cherry-pick, revert and am: new commits on the current branch.
func ruleReplay(g *gitRun) Decision {
	if g.sub == "am" && slices.Contains(g.args, "--show-current-patch") {
		return allowRead()
	}
	return g.onOwnBranch("adds commits to", "git "+g.sub)
}

func ruleStash(g *gitRun) Decision {
	sub, rest := "push", g.args
	if len(g.args) > 0 && !strings.HasPrefix(g.args[0], "-") {
		sub, rest = g.args[0], g.args[1:]
	}
	switch sub {
	case "list", "show":
		return allowRead()
	case "push", "save":
		if slices.ContainsFunc(rest, func(a string) bool { return a == "-p" || a == "--patch" }) {
			return user("interactive git stash")
		}
		return g.write("git stash " + sub)
	case "apply", "pop", "create", "store", "branch":
		// Another worktree's agent may own the entry it takes.
		return user("the stash is shared with other worktrees")
	case "drop", "clear":
		return destructive("the stash is shared with other worktrees")
	}
	return user("unknown git stash command " + sub)
}

var fetchSpec = optSpec{
	short: "jo",
	long: []string{"depth", "deepen", "shallow-since", "shallow-exclude", "refmap", "jobs", "negotiation-tip",
		"server-option", "upload-pack", "recurse-submodules-default", "filter", "submodule-prefix", "strategy",
		"strategy-option", "message", "file"},
	flags: []string{"all", "multiple", "update-head-ok", "force", "prune", "prune-tags", "tags", "no-tags",
		"dry-run", "append", "atomic", "rebase", "no-rebase", "ff-only", "squash", "unshallow", "autostash"},
}

// fetchRefs checks the refspecs of fetch or pull for local branches they
// would update.
func (g *gitRun) fetchRefs(o opts) (Decision, []string, bool) {
	if _, ok := o.val("--upload-pack"); ok {
		return user("--upload-pack runs a program"), nil, false
	}
	if o.has("-u", "--update-head-ok", "--refmap") {
		return user("git " + g.sub + " updates the checked-out branch"), nil, false
	}
	if o.has("--all", "--multiple") || len(o.pos) < 2 {
		return Decision{}, nil, true
	}
	var refs []string
	for _, spec := range o.pos[1:] {
		force := strings.HasPrefix(spec, "+")
		spec = strings.TrimPrefix(spec, "+")
		_, dst, ok := strings.Cut(spec, ":")
		if !ok || dst == "" {
			continue
		}
		if strings.HasPrefix(dst, "refs/remotes/") || strings.HasPrefix(dst, "refs/tags/") {
			continue
		}
		if strings.Contains(dst, "*") {
			return user("git " + g.sub + " refspec " + spec + " updates local branches"), nil, false
		}
		if strings.HasPrefix(dst, "refs/") && !strings.HasPrefix(dst, "refs/heads/") {
			return user("git " + g.sub + " updates " + dst), nil, false
		}
		if g.protected(dst) {
			return protectedRef("updates", dst), nil, false
		}
		if force || o.has("-f", "--force") {
			return destructive("force-updates branch "+shortBranch(dst), shortBranch(dst)), nil, false
		}
		refs = append(refs, shortBranch(dst))
	}
	return Decision{}, refs, true
}

func ruleFetch(g *gitRun) Decision {
	o := parseOpts(g.args, fetchSpec)
	if o.has("--dry-run") {
		return allowRead()
	}
	d, refs, ok := g.fetchRefs(o)
	if !ok {
		return d
	}
	return g.write("fetches", refs...)
}

func rulePull(g *gitRun) Decision {
	o := parseOpts(g.args, fetchSpec)
	d, refs, ok := g.fetchRefs(o)
	if !ok {
		return d
	}
	if o.has("-f", "--force") {
		return destructive("git pull --force")
	}
	d = g.onOwnBranch("pulls into", "pulls into the current branch")
	if d.Verdict == Allow {
		d.Refs = append(d.Refs, refs...)
	}
	return d
}

func rulePush(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{
		short: "o",
		long:  []string{"repo", "receive-pack", "exec", "push-option"},
		flags: []string{"force", "force-with-lease", "force-if-includes", "mirror", "all", "branches", "tags", "follow-tags",
			"delete", "prune", "dry-run", "set-upstream", "no-verify", "verify", "porcelain", "quiet", "verbose", "atomic",
			"progress", "thin", "no-thin", "signed", "no-signed", "recurse-submodules", "ipv4", "ipv6"},
	})
	switch {
	case o.has("-n", "--dry-run"):
		return allowRead()
	case o.has("--receive-pack", "--exec"):
		return user("git push --receive-pack runs a program")
	case o.has("--repo"):
		return user("can't parse git push --repo")
	case o.has("--mirror", "--all", "--branches"):
		return Decision{Verdict: User, Reason: "pushes every branch", Destructive: o.has("--mirror")}
	case o.has("--prune"):
		return destructive("git push --prune deletes remote branches")
	case o.has("--tags", "--follow-tags"):
		return user("pushes tags")
	}
	force := o.has("-f") || o.hasPrefix("--force")
	for n := range o.set {
		// An ambiguous abbreviation such as --forc: git refuses it, but
		// it is never routine.
		force = force || len(n) > 3 && strings.HasPrefix("--force", n)
	}
	del := o.has("-d", "--delete")
	specs := o.pos
	if len(specs) > 0 {
		specs = specs[1:] // the remote
	}
	if len(specs) == 0 {
		if del {
			return user("can't parse git push --delete")
		}
		specs = []string{"HEAD"}
	}
	var refs []string
	for _, spec := range specs {
		f := force || strings.HasPrefix(spec, "+")
		spec = strings.TrimPrefix(spec, "+")
		if strings.Contains(spec, "*") {
			return user("pushes a refspec pattern")
		}
		src, dst, hasDst := strings.Cut(spec, ":")
		d := del
		switch {
		case del:
			dst = spec
		case hasDst && src == "":
			d = true
		case !hasDst || dst == "":
			dst = src
		}
		current := !hasDst && !d && (dst == "HEAD" || dst == "@")
		if current {
			cur, err := g.current()
			if err != nil {
				return user("can't tell which branch is checked out")
			}
			if cur == "" {
				return user("pushes a detached HEAD")
			}
			dst = cur
		}
		if dst == "HEAD" || dst == "@" || strings.HasPrefix(dst, "refs/") && !strings.HasPrefix(dst, "refs/heads/") {
			return user("pushes to " + dst)
		}
		if g.protected(dst) {
			r := protectedRef("pushes to", dst)
			r.Destructive = d || f
			return r
		}
		if !hasDst && !d && !current && !g.branchExists(dst) {
			// A tag, a commit or a typo: only branches are routine.
			return user("pushes " + dst + ", which isn't a local branch")
		}
		dst = shortBranch(dst)
		if d {
			return destructive("deletes remote branch "+dst, dst)
		}
		if f {
			return destructive("force-pushes "+dst, dst)
		}
		refs = append(refs, dst)
	}
	return g.write("pushes "+strings.Join(refs, ", "), refs...)
}

func ruleReset(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{
		long:  []string{"pathspec-from-file"},
		flags: []string{"soft", "mixed", "hard", "merge", "keep", "patch", "quiet", "intent-to-add", "recurse-submodules", "no-refresh"},
	})
	cur, err := g.current()
	if o.has("--hard", "--merge", "--keep") {
		d := destructive("git reset --hard/--merge/--keep discards changes")
		if err == nil && cur != "" && g.protected(cur) {
			d.Protected = true
			d.Refs = []string{cur}
		}
		return d
	}
	if o.has("-p", "--patch") {
		return user("interactive git reset")
	}
	commit := ""
	paths := o.paths
	switch {
	case len(o.pos) == 0:
	case o.dash || g.c.lk.RefExists(o.pos[0]):
		commit = o.pos[0]
		paths = append(paths, o.pos[1:]...)
	default:
		paths = append(paths, o.pos...)
	}
	if len(paths) > 0 || o.has("--pathspec-from-file") {
		return g.write("unstages files")
	}
	if commit == "" || commit == "HEAD" || commit == "@" {
		return g.write("unstages files")
	}
	if err != nil {
		return user("can't tell which branch is checked out")
	}
	if cur == "" {
		return g.write("moves a detached HEAD")
	}
	if g.protected(cur) {
		return protectedRef("resets", cur)
	}
	if g.c.lk.Pushed(cur) {
		return Decision{Verdict: User, Reason: "resets pushed branch " + cur, Refs: []string{cur}}
	}
	return g.write("resets "+cur+" to "+commit, cur)
}

func ruleRestore(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{
		short: "s",
		long:  []string{"source", "pathspec-from-file", "conflict"},
		flags: []string{"staged", "worktree", "patch", "ours", "theirs", "merge", "overlay", "no-overlay", "ignore-unmerged"},
	})
	if o.has("-S", "--staged") && !o.has("-W", "--worktree", "-p", "--patch") {
		return g.write("unstages files")
	}
	return destructive("git restore discards working-tree changes")
}

func ruleClean(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{short: "e", long: []string{"exclude"}, flags: []string{"dry-run", "force", "interactive", "quiet"}})
	if o.has("-n", "--dry-run") && !o.has("-i", "--interactive") {
		return allowRead()
	}
	return destructive("git clean deletes untracked files")
}

func ruleTag(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{
		short: "mFu",
		long:  []string{"message", "file", "local-user", "contains", "no-contains", "points-at", "merged", "no-merged", "sort", "format", "cleanup"},
		flags: []string{"list", "delete", "verify", "force", "annotate", "sign"},
	})
	switch {
	case o.has("-d", "--delete"):
		return destructive("deletes a tag")
	case o.has("-v", "--verify"):
		return allowRead()
	case o.has("-f", "--force"):
		return destructive("moves an existing tag")
	case len(o.pos) == 0 || o.has("-l", "--list") || o.hasPrefix("-n"):
		return allowRead()
	}
	return user("creates a tag")
}

func ruleRemote(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{})
	if len(o.pos) == 0 || o.pos[0] == "show" || o.pos[0] == "get-url" {
		return allowRead()
	}
	return user("changes remotes")
}

func ruleWorktree(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{})
	if len(o.pos) == 0 {
		return user("can't parse git worktree")
	}
	switch o.pos[0] {
	case "list":
		return allowRead()
	case "remove", "prune":
		return destructive("git worktree " + o.pos[0] + " deletes worktrees")
	}
	return user("changes worktrees")
}

func ruleConfig(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{
		short: "f",
		long:  []string{"file", "blob", "type", "default", "comment", "value", "fixed-value"},
		flags: []string{"get", "get-all", "get-regexp", "get-urlmatch", "list", "add", "unset", "unset-all", "replace-all",
			"rename-section", "remove-section", "edit", "global", "system", "local", "worktree", "show-origin", "show-scope", "name-only", "null", "includes", "no-includes", "bool", "int", "path"},
	})
	if o.has("-e", "--edit", "--add", "--unset", "--unset-all", "--replace-all", "--rename-section", "--remove-section") {
		return user("changes git config")
	}
	if o.has("--get", "--get-all", "--get-regexp", "--get-urlmatch", "-l", "--list") {
		return allowRead()
	}
	if len(o.pos) > 0 {
		switch o.pos[0] {
		case "get", "list":
			return allowRead()
		case "set", "unset", "rename-section", "remove-section", "edit":
			return user("changes git config")
		}
	}
	if len(o.pos) == 1 {
		return allowRead()
	}
	return user("changes git config")
}

func ruleReflog(g *gitRun) Decision {
	if len(g.args) > 0 {
		switch g.args[0] {
		case "expire", "delete", "drop":
			return destructive("git reflog " + g.args[0] + " deletes history")
		}
	}
	return allowRead()
}

func ruleSymbolicRef(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{short: "m", flags: []string{"delete", "quiet", "short", "no-recurse", "recurse"}})
	if o.has("-d", "--delete") {
		return destructive("deletes a symbolic ref")
	}
	if len(o.pos) <= 1 {
		return allowRead()
	}
	if g.protected(o.pos[1]) {
		return protectedRef("points "+o.pos[0]+" at", o.pos[1])
	}
	return user("git symbolic-ref changes " + o.pos[0])
}

func ruleUpdateRef(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{short: "m", flags: []string{"delete", "stdin", "no-deref", "create-reflog"}})
	if len(o.pos) > 0 {
		ref := o.pos[0]
		if strings.HasPrefix(ref, "refs/heads/") || !strings.HasPrefix(ref, "refs/") {
			if g.protected(ref) {
				d := protectedRef("updates", ref)
				d.Destructive = o.has("-d", "--delete")
				return d
			}
		}
	}
	if o.has("-d", "--delete") {
		return destructive("deletes a ref with git update-ref")
	}
	return user("git update-ref changes refs directly")
}

func ruleApply(g *gitRun) Decision {
	o := parseOpts(g.args, optSpec{long: []string{"directory", "exclude", "include", "whitespace", "build-fake-ancestor"},
		flags: []string{"check", "stat", "numstat", "summary", "apply", "unsafe-paths", "index", "cached", "3way", "reverse"}})
	if o.has("--unsafe-paths") {
		return user("git apply --unsafe-paths writes outside the worktree")
	}
	if o.has("--check", "--stat", "--numstat", "--summary") && !o.has("--apply") {
		return allowRead()
	}
	return g.write("applies a patch")
}

func ruleFormatPatch(g *gitRun) Decision {
	if slices.Contains(g.args, "--stdout") {
		return ruleRead(g)
	}
	return user("git format-patch writes patch files")
}

func ruleArchive(g *gitRun) Decision {
	for _, a := range g.args {
		if a == "-o" || strings.HasPrefix(a, "--output") || strings.HasPrefix(a, "--remote") || strings.HasPrefix(a, "--exec") {
			return user("git archive " + a)
		}
	}
	return allowRead()
}

func ruleMergeFile(g *gitRun) Decision {
	if slices.Contains(g.args, "-p") || slices.Contains(g.args, "--stdout") {
		return allowRead()
	}
	return user("git merge-file rewrites a file")
}

func ruleHashObject(g *gitRun) Decision {
	if slices.Contains(g.args, "-w") {
		return user("git hash-object -w writes objects")
	}
	return allowRead()
}

func ruleFsck(g *gitRun) Decision {
	if slices.Contains(g.args, "--lost-found") {
		return user("git fsck --lost-found writes files")
	}
	return allowRead()
}

func ruleGC(g *gitRun) Decision {
	for _, a := range g.args {
		if strings.HasPrefix(a, "--prune") && a != "--prune=never" || a == "--aggressive" {
			return destructive("git gc " + a + " deletes unreachable objects")
		}
	}
	return user("runs git gc")
}
