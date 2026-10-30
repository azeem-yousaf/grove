package ui

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/plan"
)

func TestPlanChecksArePreApproved(t *testing.T) {
	m, p, _ := groupedPlan(t)
	m.cfg.Planner.Test = ""
	p.Test = "go vet ./... && go test ./... -count=1"
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	s.allow = []string{agent.ClaudeRule("Bash", "make lint")}
	allow := m.agentRequest(s).Allow
	for _, want := range []string{"Bash(go vet:*)", "Bash(go test:*)", "Bash(make lint)"} {
		if !slices.Contains(allow, want) {
			t.Errorf("allow = %q, missing %q", allow, want)
		}
	}
	if len(s.allow) != 1 {
		t.Errorf("pre-approved checks aren't saved as approvals: %q", s.allow)
	}

	// Interpreters get their exact command only, and git nothing.
	p.Test = "npx jest; git status && sh run-tests.sh"
	allow = m.agentRequest(s).Allow
	if !slices.Contains(allow, "Bash(npx jest)") || !slices.Contains(allow, "Bash(sh run-tests.sh)") {
		t.Errorf("allow = %q", allow)
	}
	for _, r := range allow {
		if strings.Contains(r, ":*") || strings.Contains(r, "git") {
			t.Errorf("unexpected rule %q in %q", r, allow)
		}
	}

	// The planner's read-only session gets none.
	p.Test = "go test ./..."
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	if allow := m.agentRequest(m.planner).Allow; len(allow) != 0 {
		t.Errorf("planner allow = %q", allow)
	}
	// Nor does an agent outside the plan.
	if got := m.planChecks(&session{path: "/elsewhere"}); len(got) != 0 {
		t.Errorf("checks outside a plan = %q", got)
	}
}

func TestStaleApprovals(t *testing.T) {
	m := fakeModel(t, 120, 40)
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	path := filepath.FromSlash("/src/shop-auth")
	s := m.newSession(path, "feature/auth", agent.Claude)
	m.plans = []*plan.Plan{{ID: "p1", Title: "Shop", Status: plan.Running, Orchestrated: true, Agent: "claude",
		Tasks: []plan.Task{{ID: "a", Branch: "feature/auth", Title: "Auth", Path: path, Status: plan.Working}}}}
	a := m.newApproval(s, "Bash", "npm test")
	s.pending = []pendingAction{a}
	if a.head != "abc1234" || a.files == "" || m.staleApproval(s, a) {
		t.Fatalf("a new approval marks the worktree and isn't stale: %+v", a)
	}
	ask := func() string {
		got, _ := m.orchPrompt(m.planner, []orchItem{{kind: orchApproval, plan: "p1", branch: "feature/auth", lines: []string{a.text}, ids: []string{a.id}}})
		return got
	}
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	if strings.Contains(ask(), staleNote) {
		t.Error("an unchanged worktree isn't stale")
	}

	// The agent edits a file: the card is stale, wherever the planner sees it.
	st := &m.snap.States[m.snap.IndexOf(path)]
	st.Files = append(slices.Clone(st.Files), git.FileChange{Path: "src/auth/test.ts", Status: git.Untracked, Added: 3, Uncommitted: true})
	if !strings.Contains(ask(), "- id "+a.id+": `npm test`"+staleNote) {
		t.Errorf("the planner should hear it is stale:\n%s", ask())
	}
	if !m.controlState().Plans[0].Tasks[0].Approvals[0].Stale {
		t.Error("state.json should mark it stale")
	}
	if len(s.pending) != 1 {
		t.Error("a stale approval isn't denied")
	}

	// The mark is saved with the session.
	back := restore(s.saved(), true, true)
	if got := back.pending[0]; got.head != a.head || got.files != a.files || got.rebase {
		t.Errorf("restored %+v", got)
	}

	// A scan started after the card was made takes the worktree again, since
	// the snapshot it was made from may have missed the agent's last edits.
	s.pending[0].rebase = true
	m.markApprovals(s.pending[0].after)
	if !m.staleApproval(s, s.pending[0]) {
		t.Error("a scan already running when the card was made doesn't count")
	}
	m.markApprovals(s.pending[0].after + 1)
	if m.staleApproval(s, s.pending[0]) || s.pending[0].rebase {
		t.Errorf("the next scan should mark it again: %+v", s.pending[0])
	}

	// New commits make it stale too.
	st.Last = &git.Commit{Hash: "def5678"}
	if !m.staleApproval(s, s.pending[0]) {
		t.Error("a new HEAD should make it stale")
	}
}

func TestPrefixApproval(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	m.grant = (&grants{}).add
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	s.running = true // answers wait for the end of its turn
	test, commit, npx := m.newApproval(s, "Bash", "go test ./internal/ui -run X"), m.newApproval(s, "Bash", "git commit -m wip"), m.newApproval(s, "Bash", "npx jest")
	s.pending = []pendingAction{test, commit, npx}
	commit.policy = approvalPolicy{} // the planner may answer it
	s.pending[1].policy = commit.policy

	_, outs := m.decide(nil, decisions{Approve: []answerDecision{
		{ID: test.id, Prefix: true},
		{ID: commit.id, Prefix: true},
		{ID: npx.id, Prefix: true},
	}})
	for _, o := range outs {
		if o.result != control.Applied {
			t.Errorf("%s", o)
		}
	}
	want := []string{"Bash(go test:*)", agent.ClaudeRule("Bash", "git commit -m wip"), agent.ClaudeRule("Bash", "npx jest")}
	if !slices.Equal(s.allow, want) {
		t.Errorf("allow = %q, want %q", s.allow, want)
	}
	if strings.Contains(outs[0].detail, "exact command only") || !strings.Contains(outs[1].detail, "exact command only") || !strings.Contains(outs[2].detail, "exact command only") {
		t.Errorf("details: %q / %q / %q", outs[0].detail, outs[1].detail, outs[2].detail)
	}

	// Through the control tool too.
	s.allow = nil
	vet := m.newApproval(s, "Bash", "go vet ./internal/...")
	s.pending = []pendingAction{vet}
	m.onControl(controlMsg{orders: []control.Order{{By: control.Lead, ID: "o-1", Action: control.Approve, Task: "feature/a", Item: vet.id, Text: vet.text, Prefix: true}}})
	if !slices.Equal(s.allow, []string{"Bash(go vet:*)"}) {
		t.Errorf("allow after the control order = %q", s.allow)
	}
}
