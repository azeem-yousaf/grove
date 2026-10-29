package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
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
	if outs[0].detail != "" || !strings.Contains(outs[1].detail, "exact command only") || !strings.Contains(outs[2].detail, "exact command only") {
		t.Errorf("details: %q / %q / %q", outs[0].detail, outs[1].detail, outs[2].detail)
	}

	// Through the control tool too.
	s.allow = nil
	vet := m.newApproval(s, "Bash", "go vet ./internal/...")
	s.pending = []pendingAction{vet}
	m.onControl(controlMsg{orders: []control.Order{{ID: "o-1", Action: control.Approve, Task: "feature/a", Item: vet.id, Text: vet.text, Prefix: true}}})
	if !slices.Equal(s.allow, []string{"Bash(go vet:*)"}) {
		t.Errorf("allow after the control order = %q", s.allow)
	}
}
