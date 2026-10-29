package ui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
)

// grants records what m.grant was asked to let through, and who approved.
type grants struct {
	mu   sync.Mutex
	cmds []string
	by   []string
}

func (g *grants) add(_ *session, _, cmd string, _ approvalPolicy, by string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cmds = append(g.cmds, cmd)
	g.by = append(g.by, by)
	return nil
}

func TestStubPolicyKeepsGitWritesForTheUser(t *testing.T) {
	for _, c := range []struct {
		cmd      string
		userOnly bool
	}{
		{"npm test", false},
		{"git status && git diff --stat", false},
		{"cd sub && git log -3", false},
		{"git push origin main", true},
		{"npm test && git commit -am x", true},
		{"/usr/bin/git -c core.x=y status", true},
		{`"C:\Program Files\Git\git.exe" merge main`, true},
	} {
		if got := stubPolicy(nil, "Bash", c.cmd); got.userOnly != c.userOnly || c.userOnly && got.reason == "" {
			t.Errorf("stubPolicy(%q) = %+v", c.cmd, got)
		}
	}
}

func (g *grants) list() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.cmds)
}

// pushNeedsYou is a policy that keeps pushes for the user.
func pushNeedsYou(_ *session, _, cmd string) approvalPolicy {
	if strings.HasPrefix(cmd, "git push") {
		return approvalPolicy{userOnly: true, protected: true, reason: "pushes protected branch main"}
	}
	return approvalPolicy{}
}

func TestPlannerCantApproveWhatOnlyTheUserMay(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.termFocused = false // alerts are only sent while the user is away
	m.classify = pushNeedsYou
	g := &grants{}
	m.grant = g.add
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	fake.turns = [][]agent.Event{
		plannerSays(`{"approve": [{"branch": "feature/a", "command": "npm test"}, {"branch": "feature/a", "command": "git push origin main"}]}`),
		{{Kind: agent.EvText, Text: "Tests pass; the push waits for you."}, {Kind: agent.EvDone}},
	}
	s.running = true
	pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{
		{Tool: "Bash", Detail: "npm test"}, {Tool: "Bash", Detail: "git push origin main"},
	}}))

	if len(fake.reqs) != 2 {
		t.Fatalf("want the planner's turn and the agent resumed, got %d requests", len(fake.reqs))
	}
	if ask := fake.reqs[0].Message; !strings.Contains(ask, "`npm test`") || strings.Contains(ask, "git push origin main") {
		t.Errorf("the planner should be asked only about what it may answer:\n%s", ask)
	}
	if len(s.pending) != 1 || s.pending[0].text != "git push origin main" || !s.pending[0].policy.userOnly {
		t.Fatalf("the push should wait for the user: %+v", s.pending)
	}
	// It carries on without waiting for the user's item, and knows about it.
	resume := fake.reqs[1].Message
	if !strings.Contains(resume, "Approved: `npm test`") || !strings.Contains(resume, "Still waiting for the user: `git push origin main`") {
		t.Errorf("agent resumed with %q", resume)
	}
	if out := strings.Join(m.chatBottom(s, 120), "\n"); !strings.Contains(out, "needs you: pushes protected branch main") {
		t.Errorf("approval card:\n%s", out)
	}
	if !slices.ContainsFunc(m.alerts, func(a string) bool { return strings.Contains(a, "needs you: pushes protected branch main") }) {
		t.Errorf("alerts = %q", m.alerts)
	}
	if !slices.ContainsFunc(m.planner.entries, func(e chatEntry) bool {
		return strings.Contains(e.text, "Not applied: approve `git push origin main` for feature/a: needs the user")
	}) || len(m.orch.reports) != 1 {
		t.Errorf("the refusal should be reported to the planner: entries %+v, reports %q", m.planner.entries, m.orch.reports)
	}
	if got := g.list(); !slices.Equal(got, []string{"npm test"}) {
		t.Errorf("grants = %q", got)
	}
	log, _ := os.ReadFile(filepath.Join(m.store.Dir(), "approvals.jsonl"))
	if !strings.Contains(string(log), `"by":"planner"`) || !strings.Contains(string(log), `"by":"grove-policy"`) || !strings.Contains(string(log), `"outcome":"needs the user"`) {
		t.Errorf("approvals.jsonl:\n%s", log)
	}

	// The user approves the push later, and the agent carries on again.
	m.chatPath, m.chatPlanner, m.focus = s.path, false, focusChat
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)
	if len(fake.reqs) < 3 || fake.reqs[2].Message != "I approved `git push origin main`. Run it now and carry on." {
		t.Fatalf("after the user's answer: %d requests, last %q", len(fake.reqs), fake.reqs[len(fake.reqs)-1].Message)
	}
	// The resumed agent's turn ends without a question, so the planner checks
	// its work, and hears once about the approval it couldn't give.
	if len(fake.reqs) != 4 || !fake.reqs[3].ReadOnly || !strings.Contains(fake.reqs[3].Message, "earlier decisions didn't apply") || len(m.orch.reports) != 0 {
		t.Errorf("planner follow-up: %d requests, reports left %q", len(fake.reqs), m.orch.reports)
	}
	if got := g.list(); !slices.Equal(got, []string{"npm test", "git push origin main"}) || !slices.Equal(g.by, []string{byPlanner, byUser}) {
		t.Errorf("grants = %q by %q", got, g.by)
	}
	if !slices.Contains(fake.reqs[2].Allow, agent.ClaudeRule("Bash", "git push origin main")) {
		t.Errorf("the Claude rule should still be added: %v", fake.reqs[2].Allow)
	}
}

func TestPlannerLeavesSomeForTheUser(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.termFocused = false
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	fake.turns = [][]agent.Event{
		plannerSays(`{"approve": [{"branch": "feature/a", "command": "npm test"}], "deny": [{"branch": "feature/a", "command": "rm -rf dist", "reason": "use npm run clean"}]}`),
	}
	s.running = true
	pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{
		{Tool: "Bash", Detail: "npm test"}, {Tool: "Bash", Detail: "rm -rf dist"}, {Tool: "Bash", Detail: "make deploy"},
	}}))
	if len(fake.reqs) != 2 {
		t.Fatalf("want the planner's turn and the agent resumed, got %d requests", len(fake.reqs))
	}
	resume := fake.reqs[1].Message
	for _, want := range []string{"Approved: `npm test`", "Denied: `rm -rf dist` (use npm run clean)", "Still waiting for the user: `make deploy`"} {
		if !strings.Contains(resume, want) {
			t.Errorf("resume is missing %q: %q", want, resume)
		}
	}
	if len(s.pending) != 1 || !s.pending[0].left {
		t.Fatalf("pending = %+v", s.pending)
	}
	if !slices.ContainsFunc(m.alerts, func(a string) bool { return strings.Contains(a, "the planner left for you") }) {
		t.Errorf("alerts = %q", m.alerts)
	}
	if !slices.ContainsFunc(m.alerts, func(a string) bool { return strings.Contains(a, "The planner denied") }) {
		t.Errorf("a denial should be told to the user: %q", m.alerts)
	}
	if out := strings.Join(m.chatBottom(s, 120), "\n"); !strings.Contains(out, "the planner left it for you") {
		t.Errorf("approval card:\n%s", out)
	}
}

func TestDecisionsByIDAndRepeated(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	g := &grants{}
	m.grant = g.add
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	a1, a2 := m.newApproval(s, "Bash", "npm test"), m.newApproval(s, "Bash", "echo `date` &&  make")
	s.pending = []pendingAction{a1, a2}
	if a1.id == a2.id || !strings.HasPrefix(a2.id, a1.id[:6]) {
		t.Fatalf("ids %q and %q", a1.id, a2.id)
	}

	cmd, outs := m.decide(decisions{Approve: []answerDecision{
		{ID: a1.id},
		{ID: a1.id}, // the same again
		{Branch: "feature/a", Command: "echo \\`date\\` && make"}, // a lost backtick escape and other spacing still match
		{Branch: "feature/a", ID: "nope-1"},
	}})
	pump(t, m, cmd)
	var got []string
	for _, o := range outs {
		got = append(got, o.result)
	}
	if want := []string{control.Applied, control.AlreadyApplied, control.Applied, control.NotFound}; !slices.Equal(got, want) {
		t.Errorf("outcomes = %q, want %q", got, want)
	}
	if n := len(g.list()); n != 2 {
		t.Errorf("each command should be granted once, got %q", g.list())
	}
	turns := taskTurns(fake)
	if len(turns) != 1 || !strings.Contains(turns[0].Message, "Approved: `npm test`, `echo `date` &&  make`") {
		t.Fatalf("agent turns = %+v", turns)
	}

	// An unmatched decision in a block is noted and told in the next request.
	m.applyDecisions("```grove-orchestrate\n{\"deny\": [{\"branch\": \"feature/a\", \"id\": \"nope-2\"}]}\n```")
	if last := m.planner.entries[len(m.planner.entries)-1]; !strings.Contains(last.text, "Not applied: deny nope-2 for feature/a: not found") {
		t.Errorf("planner chat ends with %+v", last)
	}
	ask, _ := m.orchPrompt([]orchItem{{kind: orchGroup, plan: p.ID, group: "g", lines: []string{"feature/b: hi"}}})
	if !strings.Contains(ask, "earlier decisions didn't apply") || !strings.Contains(ask, "deny nope-2 for feature/a: not found") {
		t.Errorf("next request:\n%s", ask)
	}

	// An order carried out twice changes nothing the second time.
	s.running = true // it gets its answers when this turn ends
	a3 := m.newApproval(s, "Bash", "make lint")
	s.pending = []pendingAction{a3}
	o := control.Order{ID: "o-1", Action: control.Approve, Task: "feature/a", Item: a3.id, Text: "make lint"}
	pump(t, m, m.onControl(controlMsg{orders: []control.Order{o}}))
	pump(t, m, m.onControl(controlMsg{orders: []control.Order{o}}))
	dir := control.Dir(m.store.Dir())
	if r, ok := control.ResultOf(dir, "o-1"); !ok || r.Outcome != control.AlreadyApplied {
		t.Errorf("result of the repeated order = %+v", r)
	}
	if n := len(g.list()); n != 3 {
		t.Errorf("grants = %q", g.list())
	}
	if len(s.approved) != 1 {
		t.Errorf("approved once: %q", s.approved)
	}
}

func TestApprovalIDsAreSaved(t *testing.T) {
	m, p, _ := groupedPlan(t)
	m.classify = pushNeedsYou
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	s.pending = []pendingAction{m.newApproval(s, "Bash", "git push origin main")}
	s.answered = []answerRec{{ID: "x-1", Text: "npm test", Approve: true, By: byPlanner}}
	r := restore(s.saved(), true, true)
	if len(r.pending) != 1 || r.pending[0].id != s.pending[0].id || r.pending[0].policy.reason != "pushes protected branch main" || !r.pending[0].policy.userOnly {
		t.Errorf("restored pending = %+v", r.pending)
	}
	if r.seq != s.seq || len(r.answered) != 1 || r.answered[0] != s.answered[0] {
		t.Errorf("restored seq %d, answered %+v", r.seq, r.answered)
	}
	if next := r.nextApprovalID(); next == s.pending[0].id {
		t.Errorf("a new ID repeats an old one: %s", next)
	}
}
