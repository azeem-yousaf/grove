package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
)

func suggests(line string) agent.Event {
	return agent.Event{Kind: agent.EvText, Text: "Please run this for me.\n\n```grove-run\n" + line + "\n```"}
}

// The planner answers a watched task's command cards: approving runs the
// command in the task's worktree and gives the agent the output.
func TestPlannerRunsCommandCards(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	c := m.sessionFor(m.taskPath(p, "feature/c"))
	c.lastReply.Reset()
	fake.turns = [][]agent.Event{
		plannerSays(`{"approve": [{"branch": "feature/c", "command": "echo built-by-grove"}]}`),
	}
	c.running = true
	ev := suggests("echo built-by-grove")
	c.lastReply.WriteString(ev.Text)
	c.entries = append(c.entries, chatEntry{roleAgent, ev.Text})
	cmd := m.onAgentDone(c, agent.Event{Kind: agent.EvDone})
	if len(c.pending) != 1 || c.pending[0].id == "" {
		t.Fatalf("pending %+v", c.pending)
	}
	// The card is listed for the planner's tools while it waits.
	st := m.controlState()
	if i := slices.IndexFunc(st.Plans[0].Tasks, func(task control.Task) bool { return task.Branch == "feature/c" }); i < 0 ||
		len(st.Plans[0].Tasks[i].Approvals) != 1 || !st.Plans[0].Tasks[i].Approvals[0].Run || !st.Plans[0].Tasks[i].Approvals[0].Planner {
		t.Errorf("control state: %+v", st.Plans[0].Tasks)
	}
	pump(t, m, cmd)
	if len(fake.reqs) < 2 || !strings.Contains(fake.reqs[0].Message, "`echo built-by-grove` (asks grove to run it") {
		t.Fatalf("the planner should be asked about the card: %d requests", len(fake.reqs))
	}
	if len(c.pending) != 0 || !strings.Contains(fake.reqs[1].Message, "grove ran it in your worktree") || !strings.Contains(fake.reqs[1].Message, "built-by-grove") || fake.reqs[1].Dir != c.path {
		t.Errorf("pending %+v, agent told %q", c.pending, fake.reqs[1].Message)
	}
	if !slices.ContainsFunc(c.answered, func(r answerRec) bool { return r.Text == "echo built-by-grove" && r.Approve && r.By == byPlanner }) {
		t.Errorf("answered %+v", c.answered)
	}
}

// Denying a card dismisses it and tells the agent.
func TestPlannerDismissesCommandCards(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	c := m.sessionFor(m.taskPath(p, "feature/c"))
	fake.turns = [][]agent.Event{
		plannerSays(`{"deny": [{"branch": "feature/c", "command": "make deploy", "reason": "not in this plan"}]}`),
	}
	ev := suggests("make deploy")
	c.lastReply.WriteString(ev.Text)
	c.running = true
	pump(t, m, m.onAgentDone(c, agent.Event{Kind: agent.EvDone}))
	if len(c.pending) != 0 || len(fake.reqs) < 2 || !strings.Contains(fake.reqs[1].Message, "Denied: `make deploy` (not in this plan)") {
		t.Errorf("pending %+v, requests %d", c.pending, len(fake.reqs))
	}
}

// A card only the user may run waits for them, with an alert.
func TestUserOnlyCommandCardAlerts(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.termFocused = false
	m.classify = func(*session, string, string) approvalPolicy {
		return approvalPolicy{userOnly: true, reason: "pushes protected branch main"}
	}
	c := m.sessionFor(m.taskPath(p, "feature/c"))
	c.lastReply.WriteString(suggests("make release").Text)
	c.running = true
	pump(t, m, m.onAgentDone(c, agent.Event{Kind: agent.EvDone}))
	if len(c.pending) != 1 || !c.pending[0].policy.userOnly || slices.ContainsFunc(fake.reqs, func(r agent.Request) bool { return r.ReadOnly && strings.Contains(r.Message, "make release") }) {
		t.Errorf("pending %+v, requests %d", c.pending, len(fake.reqs))
	}
	if !slices.ContainsFunc(m.alerts, func(a string) bool { return strings.Contains(a, "needs you: pushes protected branch main") }) {
		t.Errorf("alerts %q", m.alerts)
	}
}
