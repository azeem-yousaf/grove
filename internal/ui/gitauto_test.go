package ui

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/agenthook"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/gitguard"
	"github.com/azeem-yousaf/grove/internal/gitpolicy"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// fakeGuard stands in for the installed git guard: an empty hooks folder,
// so git runs as usual, verified in every worktree unless broken says why
// not. Granting is recorded.
type fakeGuard struct {
	mu     sync.Mutex
	grants []string // ref@worktree
}

func useFakeGuard(t *testing.T, m *Model, broken error) *fakeGuard {
	t.Helper()
	f := &fakeGuard{}
	dir := t.TempDir()
	m.auto.install = func(string, string, []string) (gitguard.Guard, error) { return gitguard.Guard{Dir: dir}, nil }
	m.auto.verify = func(context.Context, gitguard.Guard, string) error { return broken }
	m.auto.grant = func(_ gitguard.Guard, ref, wt string, _ time.Duration) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.grants = append(f.grants, ref+"@"+wt)
		return nil
	}
	settle(t, m, m.gitAutoTick(), func() bool {
		if !m.auto.installed {
			return false
		}
		for _, d := range m.agentDirs() {
			if _, known, _ := m.guardStatus(d); !known {
				return false
			}
		}
		return true
	})
	return f
}

// runTurn sends s one message and waits until grove has nothing more to do
// for it: the agent is idle and no git runs for it.
func runTurn(t *testing.T, m *Model, s *session, text string) {
	t.Helper()
	settle(t, m, m.send(s, text), func() bool { return !s.running && !m.gitBusy(s) })
}

func TestGroveRunsRoutineGitForAgents(t *testing.T) {
	m, p, _ := groupedPlan(t)
	useFakeGuard(t, m, nil)
	cPath := m.taskPath(p, "feature/c")
	s := m.sessionFor(cPath)
	s.kind = agent.Codex
	if err := os.WriteFile(filepath.Join(cPath, "c2.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &scripted{reply: func(req agent.Request) string {
		if req.Dir == cPath && req.Message == "commit it" {
			return "Committing.\n\n```grove-run\ngit add -A\ngit commit -m \"Add c2\"\n```"
		}
		return "Thanks."
	}}
	m.runAgent = f.run
	runTurn(t, m, s, "commit it")

	if got := gitOut(t, cPath, "log", "-1", "--format=%s"); got != "Add c2" {
		t.Fatalf("grove should have committed for the agent; last commit %q", got)
	}
	if len(s.pending) != 0 {
		t.Errorf("routine git shouldn't wait for the user: %+v", s.pending)
	}
	reqs := f.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1].Message, "grove ran your git commands") ||
		!strings.Contains(reqs[1].Message, `$ git commit -m "Add c2"  (exit 0)`) {
		t.Fatalf("the agent should hear the output: %+v", reqs)
	}
	var notes []string
	for _, e := range s.entries {
		notes = append(notes, e.text)
	}
	if !slices.Contains(notes, "grove runs git add -A") || !slices.ContainsFunc(notes, func(n string) bool { return strings.HasPrefix(n, "$ git commit") }) {
		t.Errorf("the chat should show the commands and their output:\n%s", strings.Join(notes, "\n"))
	}
	// Its task isn't finished while grove ran git for it; it is once the
	// agent replied to the output.
	if c, _ := p.Task("feature/c"); c.Status != plan.Finished {
		t.Errorf("c = %s", c.Status)
	}

	// The same turn seen again runs nothing again.
	n := len(f.requests())
	if cmd := m.suggestedLines(s, []string{"git add -A", `git commit -m "Add c2"`}); cmd != nil {
		t.Error("lines already handled for this turn ran again")
	}
	if len(f.requests()) != n {
		t.Error("no new turn expected")
	}
}

func TestProtectedGitWaitsForTheUser(t *testing.T) {
	m, p, _ := groupedPlan(t)
	useFakeGuard(t, m, nil)
	var ran []string
	m.auto.exec = func(_ context.Context, _ string, args, _ []string) ([]byte, int, error) {
		ran = append(ran, strings.Join(args, " "))
		return nil, 0, nil
	}
	aPath := m.taskPath(p, "feature/a")
	s := m.sessionFor(aPath)
	f := &scripted{reply: func(req agent.Request) string {
		return "Ready.\n\n```grove-run\ngit push origin master\ngit status\n```"
	}}
	m.runAgent = f.run
	runTurn(t, m, s, "ship it")
	if len(ran) != 0 {
		t.Errorf("nothing may run before the user answers: %v", ran)
	}
	if len(s.pending) != 2 || s.pending[0].text != "git push origin master" || !s.pending[0].policy.userOnly ||
		s.pending[0].policy.reason == "" || !s.pending[0].policy.protected {
		t.Fatalf("the push should be a card with its reason: %+v", s.pending)
	}
	// What follows a line that needs the user waits for the user too.
	if s.pending[1].text != "git status" {
		t.Errorf("pending = %+v", s.pending)
	}
	m.focus, m.chatPath = focusChat, aPath
	if out := plain(m); !strings.Contains(out, "git push origin master") {
		t.Errorf("card not shown:\n%s", out)
	}
	if len(f.requests()) != 1 {
		t.Error("the agent waits for the user")
	}
}

func TestUnguardedWorktreeNeedsTheUserForGitWrites(t *testing.T) {
	m, p, _ := groupedPlan(t)
	useFakeGuard(t, m, errFake("git is too old"))
	cPath := m.taskPath(p, "feature/c")
	s := m.sessionFor(cPath)
	if guarded, known, why := m.guardStatus(cPath); guarded || !known || why != "git is too old" {
		t.Fatalf("status = %v %v %q", guarded, known, why)
	}
	if sc := m.scopeFor(cPath); sc.Guarded {
		t.Error("an unguarded worktree's scope says guarded")
	}
	m.auto.exec = func(context.Context, string, []string, []string) ([]byte, int, error) {
		t.Error("grove ran git in an unguarded worktree")
		return nil, 0, nil
	}
	s.started = time.Now()
	m.suggestedLines(s, []string{`git commit -m "x"`})
	if len(s.pending) != 1 || !s.pending[0].policy.userOnly {
		t.Fatalf("pending = %+v", s.pending)
	}
	m.focus, m.chatPath = focusChat, cPath
	if out := plain(m); !strings.Contains(out, "unguarded: git writes need you") {
		t.Errorf("chat header should say unguarded:\n%s", out)
	}
	m.overview = true
	if out := plain(m); !strings.Contains(out, "unguarded: git writes need you (git is too old)") {
		t.Errorf("overview should say why:\n%s", out)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestScopesAreWrittenForEveryAgent(t *testing.T) {
	m, p, _ := groupedPlan(t)
	m.cfg.Git.Protected = []string{"release/*"}
	p.Orchestrated = true
	mainDir := m.plannerDir()
	m.planner = m.newPlanner(mainDir, agent.Claude)
	useFakeGuard(t, m, nil)
	dir, _ := m.controlDir()
	pol, err := agenthook.ReadPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}
	aPath, bPath, cPath := m.taskPath(p, "feature/a"), m.taskPath(p, "feature/b"), m.taskPath(p, "feature/c")
	a, ok := pol.Scope(aPath)
	if !ok || a.Branch != "feature/a" || !a.Guarded || a.Planner ||
		!slices.Equal(a.Readable, []string{bPath}) || !slices.Equal(a.Protected, []string{"main", "master", "release/*"}) {
		t.Errorf("a's scope = %+v", a)
	}
	if c, ok := pol.Scope(cPath); !ok || len(c.Readable) != 0 {
		t.Errorf("c is isolated: %+v", c)
	}
	pl, ok := pol.Scope(m.plannerAs())
	if !ok || !pl.Planner || !git.SamePath(pl.Dir, mainDir) || !slices.Equal(pl.Readable, []string{mainDir}) ||
		!slices.Contains(pl.Writable, aPath) || !slices.Contains(pl.Writable, cPath) {
		t.Errorf("planner's scope = %+v", pl)
	}
	if _, ok := pol.Scope(mainDir); ok {
		t.Error("the main worktree has no agent of its own here")
	}

	// Written only when it changes.
	os.Remove(filepath.Join(dir, "policy.json"))
	m.writePolicy()
	if _, err := os.Stat(filepath.Join(dir, "policy.json")); err == nil {
		t.Error("an unchanged policy was written again")
	}
	m.cfg.Git.Protected = nil
	m.writePolicy()
	if pol, err := agenthook.ReadPolicy(dir); err != nil || slices.Contains(pol.Agents[git.PathKey(aPath)].Protected, "release/*") {
		t.Errorf("a changed policy should be written: %+v, %v", pol, err)
	}
}

func TestRequestsRunUnderTheGuard(t *testing.T) {
	m, p, _ := groupedPlan(t)
	useFakeGuard(t, m, nil)
	aPath := m.taskPath(p, "feature/a")
	dir, _ := m.controlDir()
	req := m.agentRequest(m.sessionFor(aPath))
	if !slices.Equal(req.Env, m.auto.guard.Env()) || len(req.Env) == 0 {
		t.Errorf("env = %v", req.Env)
	}
	if want := []string{"/usr/bin/grove", "agent-hook", "--dir", dir, "--as", aPath}; !slices.Equal(req.Hook, want) {
		t.Errorf("hook = %v, want %v", req.Hook, want)
	}
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	if req := m.agentRequest(m.planner); req.Hook[len(req.Hook)-1] != m.plannerAs() || len(req.Env) == 0 {
		t.Errorf("planner request: hook %v env %v", req.Hook, req.Env)
	}
	s := m.sessionFor(m.taskPath(p, "feature/c"))
	s.kind = agent.Codex
	if req := m.agentRequest(s); req.Hook != nil || len(req.Env) == 0 {
		t.Errorf("Codex has no hook but runs under the guard: hook %v env %v", req.Hook, req.Env)
	}
}

func TestUserApprovedProtectedChangeIsGrantedAndChecked(t *testing.T) {
	m, p, _ := groupedPlan(t)
	g := useFakeGuard(t, m, nil)
	aPath := m.taskPath(p, "feature/a")
	s := m.sessionFor(aPath)
	var mu sync.Mutex
	heads := map[string]string{"refs/heads/master": "aaa", "refs/remotes/origin/master": "aaa"}
	m.auto.refHash = func(_ context.Context, _, ref string) string {
		mu.Lock()
		defer mu.Unlock()
		return heads[ref]
	}
	pol := approvalPolicy{userOnly: true, protected: true, refs: []string{"master"}, reason: "pushes to protected branch master"}

	// The planner's approval never reaches the guard.
	if err := m.gitGrant(s, "Bash", "git push origin HEAD:master", pol, byPlanner); err != nil {
		t.Fatal(err)
	}
	if len(g.grants) != 0 {
		t.Fatalf("planner approval granted the guard: %v", g.grants)
	}
	if err := m.gitGrant(s, "Bash", "git push origin HEAD:master", pol, byUser); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.grants, []string{"master@" + aPath}) {
		t.Fatalf("grants = %v", g.grants)
	}
	dir, _ := m.controlDir()
	if _, ok := agenthook.TakeGrant(dir, aPath, "git push origin HEAD:master"); !ok {
		t.Error("the hook should let the approved command through")
	}

	// The push happens in the agent's next turn; grove confirms it.
	mu.Lock()
	heads["refs/remotes/origin/master"] = "bbbbbbbbbbbb"
	mu.Unlock()
	settle(t, m, m.checkWatches(s), func() bool {
		return slices.ContainsFunc(s.entries, func(e chatEntry) bool { return strings.Contains(e.text, "Approved and done") })
	})
	if !slices.ContainsFunc(s.entries, func(e chatEntry) bool { return strings.Contains(e.text, "origin/master → bbbbbbbb") }) {
		t.Errorf("entries = %+v", s.entries)
	}

	// When nothing moved, the user hears.
	if err := m.gitGrant(s, "Bash", "git push origin HEAD:master", pol, byUser); err != nil {
		t.Fatal(err)
	}
	settle(t, m, m.checkWatches(s), func() bool {
		return slices.ContainsFunc(s.entries, func(e chatEntry) bool {
			return e.role == roleError && strings.Contains(e.text, "didn't change the protected branch")
		})
	})
	if len(m.auto.watches) != 0 {
		t.Error("checked watches should be dropped")
	}

	// A push whose branches aren't known can't be let through the guard;
	// the user is told to run it themselves.
	n := len(g.grants)
	mirror := approvalPolicy{userOnly: true, destructive: true}
	if err := m.gitGrant(s, "Bash", "git push --mirror origin", mirror, byUser); err != nil {
		t.Fatal(err)
	}
	if len(g.grants) != n || len(m.auto.watches) != 0 {
		t.Errorf("nothing to grant: grants %v", g.grants)
	}
	if last := s.entries[len(s.entries)-1]; !strings.Contains(last.text, "guard can't allow this push; run it yourself outside grove") {
		t.Errorf("last entry = %+v", last)
	}
	if pol := policyOf(gitpolicyDecision(true)); !strings.Contains(pol.reason, "run it yourself outside grove") {
		t.Errorf("card reason = %q", pol.reason)
	}
}

// gitpolicyDecision is a destructive push decision without refs.
func gitpolicyDecision(destructive bool) gitpolicy.Decision {
	return gitpolicy.Decision{Verdict: gitpolicy.User, Reason: "pushes every branch", Destructive: destructive}
}

func TestGitBriefings(t *testing.T) {
	m, p, _ := groupedPlan(t)
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	ctx := m.agentContext(s)
	for _, want := range []string{"you may run routine git yourself", "feature/a", "Never change main, master", "grove-run", "git stash"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("Claude briefing is missing %q:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "Don't run git commands that change the repository") {
		t.Error("the old rule is still there")
	}
	s.kind = agent.Codex
	if ctx := m.agentContext(s); !strings.Contains(ctx, "grove runs the routine ones") || !strings.Contains(ctx, "grove-run") {
		t.Errorf("Codex briefing:\n%s", ctx)
	}
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	if ctx := m.plannerContext(); !strings.Contains(ctx, "grove approves and runs that for them automatically") {
		t.Errorf("planner briefing:\n%s", ctx)
	}
}
