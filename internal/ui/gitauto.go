package ui

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/agenthook"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/gitguard"
	"github.com/azeem-yousaf/grove/internal/gitpolicy"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// Git autonomy: agents commit and integrate their own work, and protected
// branches (main, master, the base branch and [git] protected) only change
// with the user's approval.
//
//   - The guard (gitguard) is installed under <store>/hooks at startup and
//     whenever the protected list changes, and verified in every agent's
//     worktree before its agent starts; the result shows in the chat header
//     and the overview. Every agent runs with guard.Env().
//   - Each agent's scope (gitpolicy.Scope: its worktree and branch,
//     groupmates' worktrees readable, the protected branches, whether the
//     guard is verified) is written to the control folder's policy.json
//     (agenthook.WritePolicy), where Claude's PreToolUse hook (`grove
//     agent-hook`) reads it. The planner's scope is under a folder of its
//     own (plannerAs), since it shares the main worktree with that
//     worktree's agent: the main worktree readable, its plans' task and
//     merge worktrees writable.
//   - grove-run lines that gitpolicy allows are run by grove itself, in the
//     background, without a shell, and their output goes back to the agent;
//     the rest become cards for the user as before. Codex agents, which have
//     no hook, do all their git this way.
//   - When the user approves a command that changes a protected branch (or
//     a destructive push), the guard is granted that ref just before it runs,
//     and grove checks afterwards that the ref really moved.

// gitAuto is the Model's git autonomy state.
type gitAuto struct {
	guard      gitguard.Guard
	installed  bool     // guard is installed
	installErr error    // why it isn't
	gen        int      // counts installs: checks from an older one are stale
	protected  []string // what the installed guard protects
	installing bool

	checks    map[string]guardCheck // by git.PathKey(worktree)
	verifying map[string]bool

	policyJSON []byte          // policy.json as last written
	policyErr  string          // the last error writing it, shown once
	ran        map[string]bool // grove-run lines grove handled, by runKey
	running    map[*session]int
	watches    []refWatch // protected refs the user let an agent change

	// Hooks, replaced in tests.
	install func(storeDir, commonDir string, protected []string) (gitguard.Guard, error)
	verify  func(ctx context.Context, g gitguard.Guard, worktree string) error
	grant   func(g gitguard.Guard, ref, worktree string, ttl time.Duration) error
	lookup  func(ctx context.Context, dir string) gitpolicy.Lookup
	exec    func(ctx context.Context, dir string, args, env []string) ([]byte, int, error)
	refHash func(ctx context.Context, dir, ref string) string
}

// guardCheck is gitguard.Verify's answer for one worktree.
type guardCheck struct {
	gen int
	err error // nil: guarded
}

func newGitAuto() gitAuto {
	return gitAuto{
		checks:    map[string]guardCheck{},
		verifying: map[string]bool{},
		ran:       map[string]bool{},
		running:   map[*session]int{},
		install:   gitguard.Install,
		verify:    gitguard.Verify,
		grant: func(g gitguard.Guard, ref, worktree string, ttl time.Duration) error {
			_, err := g.Grant(ref, worktree, ttl)
			return err
		},
		lookup:  gitpolicy.GitLookup,
		exec:    git.Exec,
		refHash: git.RefHash,
	}
}

// grantTTL is how long a guard grant for an approved command lasts.
const grantTTL = 5 * time.Minute

// gitTimeout bounds each git command grove runs for an agent.
const gitTimeout = 2 * time.Minute

type (
	guardInstalledMsg struct {
		guard     gitguard.Guard
		protected []string
		err       error
	}
	guardVerifiedMsg struct {
		path string
		gen  int
		err  error
	}
	// autoGitMsg is the outcome of grove-run lines grove ran for an agent.
	autoGitMsg struct {
		path    string
		planner bool
		results []gitResult
	}
	refsCheckedMsg struct {
		watch refWatch
		now   map[string]string
	}
)

// gitResult is one git command grove ran for an agent.
type gitResult struct {
	line string
	out  string
	code int
	err  error
}

// refWatch is a protected ref change the user approved, checked once the
// agent's turn ends.
type refWatch struct {
	sess    *session
	command string
	dir     string
	before  map[string]string // ref → commit before, "" when it didn't exist
}

// protectedBranches are main, master, the base branch and [git] protected.
func (m *Model) protectedBranches() []string {
	return m.cfg.Git.ProtectedWith(localBranch(m.base))
}

// isProtectedBranch reports whether branch matches one of the protected
// names or globs, ignoring case as gitpolicy does.
func isProtectedBranch(branch string, protected []string) bool {
	b := strings.ToLower(strings.TrimPrefix(branch, "refs/heads/"))
	if b == "" {
		return false
	}
	for _, p := range protected {
		p = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(p), "refs/heads/"))
		if ok, err := path.Match(p, b); p == b || err == nil && ok {
			return true
		}
	}
	return false
}

// installGuard installs the guard when the protected branches changed since
// the last install (or there was none).
func (m *Model) installGuard() tea.Cmd {
	if m.store == nil || m.repo.CommonDir == "" || !m.baseReady || m.auto.installing {
		return nil
	}
	protected := m.protectedBranches()
	if (m.auto.installed || m.auto.installErr != nil) && slices.Equal(protected, m.auto.protected) {
		return nil
	}
	m.auto.installing = true
	install, store, common := m.auto.install, m.store.Dir(), m.repo.CommonDir
	return func() tea.Msg {
		g, err := install(store, common, protected)
		return guardInstalledMsg{guard: g, protected: protected, err: err}
	}
}

func (m *Model) onGuardInstalled(msg guardInstalledMsg) tea.Cmd {
	a := &m.auto
	a.installing = false
	a.gen++
	a.protected = msg.protected
	a.guard, a.installed, a.installErr = msg.guard, msg.err == nil, msg.err
	cmds := []tea.Cmd{m.verifyGuards()}
	if msg.err != nil {
		cmds = append(cmds, m.notify("grove couldn't install its git guard, so agents' git writes need you: "+msg.err.Error(), toastErr))
	}
	m.writePolicy()
	return tea.Batch(cmds...)
}

// gitAutoTick keeps the guard installed for the current protected
// branches, checked in every agent's worktree, and policy.json up to date
// with scans, plans and tasks. Each part only does work when something
// changed.
func (m *Model) gitAutoTick() tea.Cmd {
	return tea.Batch(m.installGuard(), m.verifyGuards(), m.writePolicy())
}

// guardEnv is what every agent's environment gets so that its git runs
// under the guard.
func (m *Model) guardEnv() []string {
	if !m.auto.installed {
		return nil
	}
	return m.auto.guard.Env()
}

// agentDirs are the worktrees whose agents need a scope: every worktree
// agent's, every started plan's task and merge worktrees, and the
// planner's.
func (m *Model) agentDirs() []string {
	var out []string
	add := func(p string) {
		if p != "" && !slices.ContainsFunc(out, func(o string) bool { return git.SamePath(o, p) }) {
			out = append(out, p)
		}
	}
	for _, s := range m.allSessions() {
		add(s.path)
	}
	for _, p := range m.plans {
		if p.Status == plan.Draft {
			continue
		}
		for _, t := range p.Tasks {
			add(t.Path)
		}
		if p.Merge != nil {
			add(p.Merge.Path)
		}
	}
	return out
}

// verifyGuards checks the guard in every agent's worktree it hasn't been
// checked in since the last install.
func (m *Model) verifyGuards() tea.Cmd {
	var cmds []tea.Cmd
	for _, dir := range m.agentDirs() {
		cmds = append(cmds, m.verifyGuard(dir))
	}
	return tea.Batch(cmds...)
}

// verifyGuard checks the guard in dir, unless that is known or under way.
func (m *Model) verifyGuard(dir string) tea.Cmd {
	a := &m.auto
	key := git.PathKey(dir)
	if !a.installed || a.verifying[key] {
		return nil
	}
	if c, ok := a.checks[key]; ok && c.gen == a.gen {
		return nil
	}
	a.verifying[key] = true
	verify, g, gen := a.verify, a.guard, a.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		return guardVerifiedMsg{path: dir, gen: gen, err: verify(ctx, g, dir)}
	}
}

func (m *Model) onGuardVerified(msg guardVerifiedMsg) tea.Cmd {
	key := git.PathKey(msg.path)
	delete(m.auto.verifying, key)
	if msg.gen != m.auto.gen {
		return m.verifyGuard(msg.path) // the guard was reinstalled meanwhile
	}
	prev, had := m.auto.checks[key]
	m.auto.checks[key] = guardCheck{gen: msg.gen, err: msg.err}
	if msg.err != nil && (!had || prev.err == nil || prev.err.Error() != msg.err.Error()) {
		for _, s := range m.allSessions() {
			if git.SamePath(s.path, msg.path) {
				s.entries = append(s.entries, chatEntry{roleNote, "grove's git guard isn't active in this worktree (" + msg.err.Error() + "), so the agent's git writes need you."})
			}
		}
	}
	return m.writePolicy()
}

// guardStatus says whether dir's git runs under a verified guard, and if
// not, why. known is false while that isn't known yet.
func (m *Model) guardStatus(dir string) (guarded, known bool, why string) {
	a := &m.auto
	switch {
	case a.installErr != nil:
		return false, true, "the guard couldn't be installed: " + a.installErr.Error()
	case !a.installed:
		return false, false, ""
	}
	c, ok := a.checks[git.PathKey(dir)]
	if !ok || c.gen != a.gen {
		return false, false, ""
	}
	if c.err != nil {
		return false, true, c.err.Error()
	}
	return true, true, ""
}

// guardBadge is the guard's state for dir in words, for the chat header
// and (withWhy) the overview; "" while it isn't known.
func (m *Model) guardBadge(dir string, withWhy bool) string {
	guarded, known, why := m.guardStatus(dir)
	switch {
	case !known:
		return ""
	case guarded:
		return m.st.add.Render("🛡 guarded")
	}
	out := m.st.warn.Render("unguarded: git writes need you")
	if withWhy {
		out += m.st.faint.Render(" (" + why + ")")
	}
	return out
}

// plannerAs is the folder the planner's scope and grants are kept under.
// It shares the main worktree with that worktree's own agent, so its
// worktree can't be the key.
func (m *Model) plannerAs() string {
	dir, ok := m.controlDir()
	if !ok {
		return ""
	}
	return filepath.Join(dir, "planner")
}

// hookAs is who s is to the agent hook and its grants.
func (m *Model) hookAs(s *session) string {
	if s.planner {
		return m.plannerAs()
	}
	return s.path
}

// agentHook is the PreToolUse hook command for s's agent, or nil.
func (m *Model) agentHook(s *session) []string {
	dir, ok := m.controlDir()
	if s.kind != agent.Claude || !ok || m.groveBin == "" {
		return nil
	}
	return []string{m.groveBin, "agent-hook", "--dir", dir, "--as", m.hookAs(s)}
}

// scopeFor is what the agent working in dir may do with git.
func (m *Model) scopeFor(dir string) gitpolicy.Scope {
	guarded, _, _ := m.guardStatus(dir)
	sc := gitpolicy.Scope{Worktree: dir, Protected: m.protectedBranches(), Guarded: guarded}
	if i := m.snapIndex(dir); i >= 0 {
		sc.Branch = m.snap.States[i].Branch
	}
	sc.Readable = m.groupmateDirs(&session{path: dir})
	return sc
}

// plannerScope is the planner's: it reads the main worktree and may run
// git in its plans' task and merge worktrees.
func (m *Model) plannerScope() gitpolicy.Scope {
	main := m.plannerDir()
	if m.planner != nil {
		main = m.planner.path
	}
	guarded, _, _ := m.guardStatus(main)
	sc := gitpolicy.Scope{Worktree: m.plannerAs(), Dir: main, Protected: m.protectedBranches(), Guarded: guarded}
	if main != "" {
		sc.Readable = []string{main}
	}
	for _, p := range m.plans {
		if !watched(p) {
			continue
		}
		for _, t := range p.Tasks {
			if t.Path != "" {
				sc.Writable = append(sc.Writable, t.Path)
			}
		}
		if p.Merge != nil && p.Merge.Path != "" {
			sc.Writable = append(sc.Writable, p.Merge.Path)
		}
	}
	return sc
}

// sessionScope is s's scope.
func (m *Model) sessionScope(s *session) gitpolicy.Scope {
	if s.planner {
		return m.plannerScope()
	}
	return m.scopeFor(s.path)
}

// policy is every agent's scope.
func (m *Model) policy() agenthook.Policy {
	var p agenthook.Policy
	for _, dir := range m.agentDirs() {
		if m.planner != nil && git.SamePath(dir, m.planner.path) && m.sessionFor(dir) == nil {
			continue // only the planner works there
		}
		p.Set(agenthook.AgentScope{Scope: m.scopeFor(dir)})
	}
	if as := m.plannerAs(); as != "" && m.plannerDir() != "" {
		p.Set(agenthook.AgentScope{Scope: m.plannerScope(), Planner: true})
	}
	return p
}

// writePolicy writes policy.json when the scopes changed.
func (m *Model) writePolicy() tea.Cmd {
	dir, ok := m.controlDir()
	if !ok {
		return nil
	}
	p := m.policy()
	p.Version = agenthook.PolicyVersion
	b, err := json.Marshal(p)
	if err != nil || bytes.Equal(b, m.auto.policyJSON) {
		return nil
	}
	if err := agenthook.WritePolicy(dir, p); err != nil {
		if msg := err.Error(); msg != m.auto.policyErr {
			m.auto.policyErr = msg
			return m.notify("Couldn't write agents' git policy: "+msg, toastErr)
		}
		return nil
	}
	m.auto.policyJSON, m.auto.policyErr = b, ""
	return nil
}

// gitRules tell a worktree agent what it may do with git, for its briefing.
func (m *Model) gitRules(s *session) string {
	protected := strings.Join(m.protectedBranches(), ", ")
	never := "- Never change main, master, the default branch or any other protected branch (" + protected + "): no commits, merges, rebases, resets or pushes to them. " +
		"Destructive or protected operations (discarding work, deleting branches, force pushes, anything that changes a protected branch) need the user: " +
		"ask for them in a fenced code block whose language is grove-run, one command per line, and say why.\n"
	run := "```grove-run\ngit add -A\ngit commit -m \"Describe the change\"\n```\n"
	branch := s.name
	if branch == "" {
		branch = "your branch"
	}
	if s.kind == agent.Codex {
		return "- Git: commit your work as you go. You can't run git commands that change the repository yourself, so put them in a fenced code block whose language is grove-run, one command per line: " +
			"grove runs the routine ones (add, commit, branch, switch, merging between feature branches, rebasing unpushed work, fetch, pushing " + branch + ") in your worktree right away and sends you their output; anything else waits for the user. For example:\n\n" + run +
			"\n" + never
	}
	return "- Git: you may run routine git yourself in your own worktree on your own branch, " + branch + ": add, commit, branch, switch, merging between feature branches, rebasing work that isn't pushed yet, fetch, and pushing " + branch + ". " +
		"Commit your work as you go. grove checks every git command; one that needs the user is refused or asked about, so don't try to get around it.\n" + never +
		"For example:\n\n```grove-run\ngit push origin " + branch + ":" + cmp.Or(localBranch(m.base), "main") + "\n```\n\n" +
		"- Don't use git stash (the stash is shared with other worktrees), --no-verify, or -c / GIT_CONFIG settings that change hooks.\n"
}

// plannerGitRules tell the planner how git is handled, for its briefing.
func plannerGitRules() string {
	return "- Task agents run routine git on their own branches themselves (commit, branch, merges between feature branches, pushing their own branch), " +
		"and grove approves and runs that for them automatically, Codex agents' too. Every other approval request is yours to answer: " +
		"approve what the task needs (including git that grove couldn't check on its own, once you've read it and it is safe), deny the rest with what to do instead. " +
		"Only destructive git (deleting branches, worktrees or the repository, force pushes, discarding work) and changes to protected branches " +
		"(main, master, the default branch and the configured ones) are the user's; grove won't let you approve those. " +
		"The work reaches the base branch through the plan's pull request, so no agent needs to touch a protected branch.\n"
}

// policyOf is a gitpolicy decision as an approval's policy: only the user
// may answer what changes a protected branch, could lose work, or could get
// around the git guard. Anything
// else gitpolicy doesn't allow outright (an unusual command, one it can't
// read) is the watching planner's to judge, so a plan it is in charge of
// never waits for the user over routine git.
func policyOf(d gitpolicy.Decision) approvalPolicy {
	reason := d.Reason
	if unguardable(d.Protected, d.Destructive, d.Refs) {
		reason = strings.TrimPrefix(reason+"; "+cantGuard, "; ")
	}
	return approvalPolicy{
		userOnly:    d.Verdict == gitpolicy.User && (d.Protected || d.Destructive || d.Bypass),
		reason:      reason,
		protected:   d.Protected,
		destructive: d.Destructive,
		refs:        d.Refs,
	}
}

// gitClassify is m.classify: what grove's git policy says about a command
// s's agent was refused.
func (m *Model) gitClassify(s *session, tool, command string) approvalPolicy {
	return policyOf(m.classifyFor(s, shellOf(tool), command))
}

// gitGrant is m.grant: an approved command goes through the agent's hook
// once, and, when the user approved a change to a protected branch or a
// destructive push, through the guard once for each ref it changes. The
// planner's approvals never reach the guard.
func (m *Model) gitGrant(s *session, tool, command string, pol approvalPolicy, by string) error {
	var errs []error
	if dir, ok := m.controlDir(); ok {
		errs = append(errs, agenthook.AddGrant(dir, agenthook.Grant{Worktree: m.hookAs(s), Command: command}))
	}
	switch {
	case by != byUser || !pol.protected && !pol.destructive:
	case unguardable(pol.protected, pol.destructive, pol.refs):
		s.entries = append(s.entries, chatEntry{roleError, "The " + cantGuard + ": " + command})
	default:
		errs = append(errs, m.grantProtected(s, command, pol.refs))
	}
	return errors.Join(errs...)
}

// cantGuard is said of a protected or destructive command whose branches
// aren't known (push --all, --mirror, --prune, --tags): the guard can only
// let named refs through.
const cantGuard = "guard can't allow this push; run it yourself outside grove"

func unguardable(protected, destructive bool, refs []string) bool {
	return (protected || destructive) && len(refs) == 0
}

// shellOf is the gitpolicy shell of a Claude shell tool.
func shellOf(tool string) gitpolicy.Shell {
	if tool == "PowerShell" {
		return gitpolicy.PowerShell
	}
	return gitpolicy.Bash
}

// classify is gitpolicy's decision on command for s's agent.
func (m *Model) classifyFor(s *session, sh gitpolicy.Shell, command string) gitpolicy.Decision {
	sc := m.sessionScope(s)
	dir := sc.Dir
	if dir == "" {
		dir = sc.Worktree
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	return gitpolicy.Classify(command, sh, sc, m.auto.lookup(ctx, dir))
}

// plainGit splits a grove-run line into git's arguments when grove can run
// it as it is, without a shell: one git command and nothing a shell would
// expand or join.
func plainGit(line string) ([]string, bool) {
	if strings.ContainsAny(line, ";&|<>`$\n\r") {
		return nil, false
	}
	argv := splitCommand(line)
	if len(argv) < 2 || argv[0] != "git" {
		return nil, false
	}
	return argv[1:], true
}

// runKey identifies a grove-run line of one turn of s, so it is handled
// once however often the turn's end is seen.
func (m *Model) runKey(s *session, i int, line string) string {
	return m.hookAs(s) + "\x00" + strconv.FormatInt(s.started.UnixNano(), 10) + "\x00" + strconv.Itoa(i) + "\x00" + line
}

// suggestedLines turns the grove-run lines of s's reply into cards, after
// running those gitpolicy allows. Lines after one that needs the user wait
// for the user too, since they may depend on it.
func (m *Model) suggestedLines(s *session, lines []string) tea.Cmd {
	var run []string
	needUser := false
	for i, line := range lines {
		key := m.runKey(s, i, line)
		if m.auto.ran[key] {
			continue
		}
		m.auto.ran[key] = true
		if !needUser {
			if _, ok := plainGit(line); ok {
				if d := m.classifyFor(s, gitpolicy.Bash, line); d.Verdict == gitpolicy.Allow {
					run = append(run, line)
					continue
				} else if d.Verdict == gitpolicy.User {
					s.pending = append(s.pending, pendingAction{kind: pendingCommand, text: line, policy: policyOf(d)})
					needUser = true
					continue
				}
			}
		}
		needUser = true
		s.pending = append(s.pending, pendingAction{kind: pendingCommand, text: line})
	}
	if len(run) == 0 {
		return nil
	}
	m.auto.running[s]++
	dir, env, exec, sess, planner := s.path, m.guardEnv(), m.auto.exec, s.path, s.planner
	if planner {
		dir = m.plannerDir()
	}
	for _, line := range run {
		s.entries = append(s.entries, chatEntry{roleTool, "grove runs " + line})
	}
	return func() tea.Msg {
		out := autoGitMsg{path: sess, planner: planner}
		for _, line := range run {
			args, _ := plainGit(line)
			ctx, cancel := context.WithTimeout(m.ctx, gitTimeout)
			b, code, err := exec(ctx, dir, args, env)
			cancel()
			out.results = append(out.results, gitResult{line: line, out: string(b), code: code, err: err})
			if err != nil {
				break // what follows may depend on it
			}
		}
		return out
	}
}

func (m *Model) onAutoGit(msg autoGitMsg) tea.Cmd {
	s := m.sessionFor(msg.path)
	if msg.planner {
		s = m.planner
	}
	if s == nil {
		return m.startScan()
	}
	if m.auto.running[s]--; m.auto.running[s] <= 0 {
		delete(m.auto.running, s)
	}
	var report []string
	failed := false
	for _, r := range msg.results {
		out := strings.TrimSpace(tail(r.out, 3000))
		status := "exit 0"
		if r.err != nil {
			failed = true
			status = r.err.Error()
			if r.code > 0 {
				status = "exit " + strconv.Itoa(r.code)
			}
		}
		note := "$ " + r.line + "  (" + status + ")"
		if out != "" {
			note += "\n\n```\n" + out + "\n```"
		}
		role := roleNote
		if r.err != nil {
			role = roleError
		}
		s.entries = append(s.entries, chatEntry{role, note})
		report = append(report, note)
	}
	text := "grove ran your git commands in your worktree:\n\n" + strings.Join(report, "\n\n")
	if failed {
		text += "\n\nIt stopped at the one that failed; the rest didn't run. Fix the cause and try again."
	}
	text += "\n\nCarry on with your task."
	cmds := []tea.Cmd{m.startScan()}
	if len(s.pending) > 0 || s.running {
		// It hears with its next turn, after the user answers the cards.
		s.notes = append(s.notes, text)
		return tea.Batch(cmds...)
	}
	return tea.Batch(append(cmds, m.send(s, text))...)
}

// gitBusy reports whether grove is running git for s's agent.
func (m *Model) gitBusy(s *session) bool { return m.auto.running[s] > 0 }

// grantProtected lets an approved command through the guard: once for each
// ref it changes, in worktree. Only the user's approvals reach here. It
// remembers the refs as they are, to check they moved after the agent's
// next turn.
func (m *Model) grantProtected(s *session, command string, refs []string) error {
	if !m.auto.installed || len(refs) == 0 {
		return nil
	}
	var errs []string
	wt := s.path
	if s.planner {
		wt = "" // it runs git in its plans' worktrees, not its own
	}
	for _, ref := range refs {
		if err := m.auto.grant(m.auto.guard, ref, wt, grantTTL); err != nil {
			errs = append(errs, err.Error())
		}
	}
	dir := s.path
	if s.planner {
		dir = m.plannerDir()
	}
	w := refWatch{sess: s, command: command, dir: dir, before: map[string]string{}}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	for _, ref := range refs {
		for _, r := range watchedRefs(ref) {
			w.before[r] = m.auto.refHash(ctx, dir, r)
		}
	}
	m.auto.watches = append(m.auto.watches, w)
	if len(errs) > 0 {
		return fmt.Errorf("couldn't grant the git guard: %s", strings.Join(errs, "; "))
	}
	return nil
}

// watchedRefs are the refs that move when branch does: the local branch,
// and its remote-tracking ref after a push.
func watchedRefs(branch string) []string {
	b := strings.TrimPrefix(branch, "refs/heads/")
	return []string{"refs/heads/" + b, "refs/remotes/" + prRemote + "/" + b}
}

// checkWatches checks, once s's turn ended, that the protected refs the
// user let it change moved.
func (m *Model) checkWatches(s *session) tea.Cmd {
	var cmds []tea.Cmd
	keep := m.auto.watches[:0]
	for _, w := range m.auto.watches {
		if w.sess != s {
			keep = append(keep, w)
			continue
		}
		hash := m.auto.refHash
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
			defer cancel()
			now := map[string]string{}
			for r := range w.before {
				now[r] = hash(ctx, w.dir, r)
			}
			return refsCheckedMsg{watch: w, now: now}
		})
	}
	m.auto.watches = keep
	return tea.Batch(cmds...)
}

func (m *Model) onRefsChecked(msg refsCheckedMsg) tea.Cmd {
	w := msg.watch
	var moved []string
	for r, before := range w.before {
		if now := msg.now[r]; now != before && now != "" {
			moved = append(moved, strings.TrimPrefix(strings.TrimPrefix(r, "refs/heads/"), "refs/remotes/")+" → "+shortHash(now))
		}
	}
	slices.Sort(moved)
	if len(moved) > 0 {
		w.sess.entries = append(w.sess.entries, chatEntry{roleNote, "Approved and done: " + w.command + " (" + strings.Join(moved, ", ") + ")"})
		return nil
	}
	text := "The approved command didn't change the protected branch: " + w.command
	w.sess.entries = append(w.sess.entries, chatEntry{roleError, text + ". Check the agent's output; it may have failed or not run it."})
	return tea.Batch(m.notify(w.sess.name+": "+text, toastErr), m.alert(w.sess.name+": "+text))
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
