package gitpolicy

import (
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

type classifier struct {
	sc       Scope
	lk       Lookup
	sh       Shell
	cwd      string
	cwdKnown bool
	taint    string            // why git later in the line can't be trusted
	mentions bool              // the whole line mentions git somewhere
	moved    map[string]string // branch a worktree root switched to earlier in the line
	out      Decision
}

func (c *classifier) run(line string) Decision {
	c.mentions = mentionsGit(line)
	s, err := parse(line, c.sh)
	if err != nil {
		if c.mentions {
			return user("can't parse the command reliably")
		}
		return Decision{}
	}
	c.cwd = c.sc.Dir
	if c.cwd == "" {
		c.cwd = c.sc.Worktree
	}
	c.cwdKnown = c.cwd != ""
	c.moved = map[string]string{}
	c.script(s)
	return c.out
}

func user(reason string) Decision { return Decision{Verdict: User, Reason: reason} }

// add folds one command's decision into the line's: any User wins, and the
// first User's reason is the one shown.
func (c *classifier) add(d Decision) {
	if d.Verdict == NotGit {
		return
	}
	o := &c.out
	o.Protected = o.Protected || d.Protected
	o.Destructive = o.Destructive || d.Destructive
	for _, r := range d.Refs {
		if !slices.Contains(o.Refs, r) {
			o.Refs = append(o.Refs, r)
		}
	}
	switch {
	case o.Verdict == NotGit, o.Verdict == Allow && d.Verdict == User:
		o.Verdict, o.Reason = d.Verdict, d.Reason
	}
}

func (c *classifier) script(s *script) {
	for i, cmd := range s.cmds {
		lo, hi := i, i
		for lo > 0 && s.cmds[lo-1].sep == "|" {
			lo--
		}
		for hi < len(s.cmds)-1 && s.cmds[hi].sep == "|" {
			hi++
		}
		c.add(c.command(cmd, s.cmds[lo:hi+1]))
	}
}

var gitWord = regexp.MustCompile(`(?i)(^|[^\w.-])git(\.exe)?(\W|$)`)

// mentionsGit reports whether text names git or a .git folder.
func mentionsGit(text string) bool {
	return gitWord.MatchString(text) || hasDotGit(text)
}

// hasDotGit reports whether text holds a path with a .git component.
func hasDotGit(text string) bool {
	for _, f := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '/' || r == '\\' || r == ' ' || r == '"' || r == '\'' || r == '=' || r == '\n' || r == ':'
	}) {
		if strings.EqualFold(f, ".git") {
			return true
		}
	}
	return false
}

func commandMentionsGit(cmd *command) bool {
	for _, w := range cmd.words {
		if mentionsGit(w.raw) || mentionsGit(w.text) {
			return true
		}
	}
	for _, r := range cmd.redirs {
		if mentionsGit(r.target.raw) {
			return true
		}
	}
	for _, h := range cmd.heredocs {
		if mentionsGit(h.body) {
			return true
		}
	}
	for _, n := range cmd.nested {
		if scriptMentionsGit(n) {
			return true
		}
	}
	return false
}

func scriptMentionsGit(s *script) bool {
	return slices.ContainsFunc(s.cmds, commandMentionsGit)
}

// cmdName is a command word reduced to the program's name: base name, lower
// case, without .exe.
func cmdName(text string) string {
	if i := strings.LastIndexAny(text, `/\`); i >= 0 {
		text = text[i+1:]
	}
	text = strings.ToLower(text)
	return strings.TrimSuffix(text, ".exe")
}

// interpreters run code given as text, a file or standard input; git inside
// them can't be seen, so a line that mentions git and uses one needs the user.
var interpreters = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true,
	"ash": true, "busybox": true, "cmd": true, "pwsh": true, "powershell": true, "eval": true, "source": true,
	".": true, "xargs": true, "find": true, "parallel": true, "watch": true, "script": true, "wsl": true,
	"python": true, "python3": true, "py": true, "node": true, "perl": true, "ruby": true, "php": true,
	"bun": true, "deno": true, "ssh": true, "sudo": true, "doas": true, "su": true, "runas": true, "gsudo": true,
	"invoke-expression": true, "iex": true, "invoke-command": true, "icm": true, "start-process": true,
	"saps": true, "start": true, "foreach-object": true, "%": true, "where-object": true, "?": true,
	"foreach": true, "invoke-item": true, "ii": true, "start-job": true, "sajb": true, "start-threadjob": true,
}

// wrappers run the rest of their arguments as a command in the same way.
var wrappers = map[string]bool{
	"command": true, "builtin": true, "exec": true, "nohup": true, "time": true, "nice": true,
	"stdbuf": true, "timeout": true, "env": true, "ionice": true, "chrt": true, "taskset": true,
}

// readers only look at files, so naming something inside .git is harmless.
var readers = map[string]bool{
	"cat": true, "ls": true, "head": true, "tail": true, "less": true, "more": true, "grep": true,
	"rg": true, "ag": true, "stat": true, "file": true, "wc": true, "du": true, "tree": true, "test": true,
	"[": true, "[[": true, "echo": true, "printf": true, "type": true, "realpath": true, "readlink": true,
	"dirname": true, "basename": true, "cd": true, "pushd": true, "get-content": true, "gc": true,
	"get-childitem": true, "gci": true, "dir": true, "test-path": true, "select-string": true, "sls": true,
	"get-item": true, "gi": true, "resolve-path": true, "write-output": true, "write-host": true,
	"diff": true, "cmp": true, "sha256sum": true, "md5sum": true, "od": true, "xxd": true, "hexdump": true,
}

// safeEnv lists variables that can't change what git does to the repository.
func safeEnv(name string) bool {
	n := strings.ToUpper(name)
	if strings.HasPrefix(n, "LC_") {
		return true
	}
	switch n {
	case "LANG", "LANGUAGE", "TZ", "TERM", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "FORCE_COLOR",
		"COLUMNS", "LINES", "PAGER", "GIT_PAGER", "LESS", "CI":
		return true
	}
	return false
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)

// envName returns the variable a PowerShell $env:NAME or ${env:NAME} sets.
func envName(raw string) (string, bool) {
	r := strings.TrimPrefix(strings.Trim(raw, "{}"), "$")
	r = strings.Trim(r, "{}")
	if len(r) > 4 && strings.EqualFold(r[:4], "env:") {
		return r[4:], true
	}
	return "", false
}

func (c *classifier) command(cmd *command, pipe []*command) Decision {
	for _, n := range cmd.nested {
		if scriptMentionsGit(n) {
			return user("runs git inside a subshell or command substitution")
		}
	}
	for _, r := range cmd.redirs {
		if strings.Contains(r.op, ">") && !strings.HasSuffix(r.op, "&") && (hasDotGit(r.target.text) || hasDotGit(r.target.raw)) {
			return user("writes into .git directly")
		}
	}
	if cmd.complex != "" && commandMentionsGit(cmd) {
		return user("can't parse the command reliably (" + cmd.complex + ")")
	}
	words := cmd.words
	if len(words) == 0 {
		return Decision{}
	}

	// PowerShell assignments: `$env:GIT_DIR = x` changes git's environment,
	// and `$x = git ...` runs git.
	if c.sh == PowerShell && strings.HasPrefix(words[0].raw, "$") && len(words) > 1 &&
		slices.Contains([]string{"=", "+=", "-=", "*=", "/=", "??="}, words[1].text) {
		if name, ok := envName(words[0].raw); ok && !safeEnv(name) {
			c.taint = "sets " + name
		}
		words = words[2:]
		if len(words) == 0 {
			return Decision{}
		}
	}

	// Bash VAR=value prefixes: for this command only, or for the rest of the
	// line when nothing follows them.
	var env []string
	if c.sh == Bash {
		// Reserved words before a command: `if git diff --quiet; then git commit`.
		for len(words) > 0 && slices.Contains([]string{"if", "then", "elif", "else", "do", "while", "until", "!"}, words[0].raw) {
			words = words[1:]
		}
		if len(words) == 0 || slices.Contains([]string{"for", "select", "case", "fi", "done", "esac"}, words[0].raw) {
			return Decision{}
		}
		for len(words) > 0 && assignment.MatchString(words[0].raw) {
			name, _, _ := strings.Cut(words[0].raw, "=")
			env = append(env, strings.TrimSuffix(name, "+"))
			words = words[1:]
		}
		if len(words) == 0 {
			for _, name := range env {
				if !safeEnv(name) {
					c.taint = "sets " + name
				}
			}
			return Decision{}
		}
	}

	// Wrappers such as `command git` or `env X=y git`.
	for len(words) > 0 && !words[0].dynamic && wrappers[cmdName(words[0].text)] {
		name := cmdName(words[0].text)
		words = words[1:]
		if name == "command" && len(words) > 0 && (words[0].text == "-v" || words[0].text == "-V") {
			return Decision{}
		}
		for len(words) > 0 && strings.HasPrefix(words[0].text, "-") {
			opt := words[0].text
			words = words[1:]
			switch {
			case name == "env" && (opt == "-i" || opt == "--ignore-environment" || opt == "-" || opt == "-S" || opt == "--split-string" || strings.HasPrefix(opt, "-C") || strings.HasPrefix(opt, "--chdir")):
				env = append(env, "the environment ("+opt+")")
			case name == "env" && (opt == "-u" || opt == "--unset"):
				if len(words) > 0 {
					env = append(env, words[0].text)
					words = words[1:]
				}
			case name == "nice" && opt == "-n", name == "timeout" && (opt == "-s" || opt == "-k"), name == "ionice" && (opt == "-c" || opt == "-n"):
				if len(words) > 0 {
					words = words[1:]
				}
			}
		}
		if name == "timeout" && len(words) > 0 {
			words = words[1:] // the duration
		}
		if name == "env" {
			for len(words) > 0 && assignment.MatchString(words[0].text) {
				n, _, _ := strings.Cut(words[0].text, "=")
				env = append(env, strings.TrimSuffix(n, "+"))
				words = words[1:]
			}
		}
		if (name == "chrt" || name == "taskset") && len(words) > 0 {
			words = words[1:]
		}
	}
	if len(words) == 0 {
		return Decision{}
	}
	if words[0].dynamic {
		if c.mentions {
			return user("runs a command taken from a variable")
		}
		return Decision{}
	}
	name := cmdName(words[0].text)
	switch {
	case name == "git":
		return c.git(cmd, words[1:], env)
	case strings.HasPrefix(name, "git-"):
		return user("runs a git program directly")
	}

	piped := cmd.sep == "|" || len(pipe) > 1
	switch name {
	case "cd", "chdir", "pushd", "set-location", "sl", "popd", "pop-location":
		if !piped && cmd.sep != "&" {
			c.cd(name, words[1:])
		}
		return Decision{}
	case "export", "declare", "typeset", "readonly", "local", "set-item", "si", "new-item", "ni", "set-content":
		for _, w := range words[1:] {
			n, _, _ := strings.Cut(w.text, "=")
			if en, ok := envName("$" + n); ok {
				n = en
			} else if strings.HasPrefix(strings.ToLower(n), "env:") {
				n = n[4:]
			} else if name != "export" && name != "declare" && name != "typeset" && name != "readonly" && name != "local" {
				continue
			}
			if !strings.HasPrefix(n, "-") && n != "" && !safeEnv(n) {
				c.taint = "sets " + n
			}
		}
	case "alias", "function", "filter", "hash", "enable", "shopt", "set-alias", "sal", "new-alias", "nal",
		"import-module", "ipmo", "add-type":
		c.taint = "changes the shell with " + name
		if commandMentionsGit(cmd) {
			return user("redefines git with " + name)
		}
	}
	if interpreters[name] {
		if slices.ContainsFunc(pipe, commandMentionsGit) {
			return user("runs git through " + name)
		}
		if name == "source" || name == "." {
			c.taint = "runs a script with " + name
		}
		return Decision{}
	}
	if !readers[name] {
		for _, w := range words[1:] {
			if hasDotGit(w.text) {
				return user("changes .git directly")
			}
		}
	}
	return Decision{}
}

// cd follows a change of folder. Anything it can't resolve makes the folder
// unknown, so git after it needs the user.
func (c *classifier) cd(name string, args []word) {
	if name == "popd" || name == "pop-location" {
		c.cwdKnown = false
		return
	}
	var target *word
	for i := 0; i < len(args); i++ {
		a := args[i]
		t := strings.ToLower(a.text)
		if t == "--" {
			if i+1 < len(args) {
				target = &args[i+1]
			}
			break
		}
		if c.sh == PowerShell && (t == "-path" || t == "-literalpath") {
			if i+1 < len(args) {
				target = &args[i+1]
			}
			break
		}
		if strings.HasPrefix(t, "-") && t != "-" {
			continue
		}
		target = &args[i]
		break
	}
	if target == nil || target.dynamic || target.text == "-" || target.text == "" {
		c.cwdKnown = false
		return
	}
	c.cwd = resolve(c.cwd, target.text)
}

// resolve turns p, relative to base, into a clean absolute path. On Windows it
// also understands Git Bash's /c/... form.
func resolve(base, p string) string {
	if runtime.GOOS == "windows" && len(p) >= 2 && p[0] == '/' && isLetter(p[1]) && (len(p) == 2 || p[2] == '/') {
		p = strings.ToUpper(p[1:2]) + ":" + filepath.FromSlash(p[2:]+"/")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
