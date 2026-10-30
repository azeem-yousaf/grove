package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/usage"
)

// Approvals don't wait for a planner deep in a long turn: its lane answers
// them, within the owner's scope, and the owner's chat hears about it.
func TestApprovalLaneAnswersWhileThePlannerIsBusy(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	withLedger(t, m)
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	m.now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	crew := m.plannerFor(p.ID, "g")
	crew.running, crew.started = true, m.now.Add(-10*time.Second)
	a := m.sessionFor(m.taskPath(p, "feature/a"))
	deny := func() {
		a.running = true
		pump(t, m, m.onAgentDone(a, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: "npm test"}}}))
	}
	deny()
	if len(fake.reqs) != 0 || len(m.lanes) != 0 {
		t.Fatalf("a planner busy for less than laneWait keeps its items: %d requests", len(fake.reqs))
	}

	// Busy for longer: its lane answers, and may only touch its group.
	m.now = crew.started.Add(laneWait)
	fake.turns = [][]agent.Event{plannerSays(`{"approve": [{"branch": "feature/a", "command": "npm test"}, {"branch": "feature/c", "command": "rm -rf /"}]}`)}
	pump(t, m, m.orchestrate())
	if len(fake.reqs) < 1 {
		t.Fatal("the lane should be asked")
	}
	req := fake.reqs[0]
	if !req.ReadOnly || req.Slot != crew.crewKey()+"-lane" || req.Dir != crew.path || !strings.Contains(req.Message, "`npm test`") {
		t.Errorf("lane request: slot %q, dir %s, message:\n%s", req.Slot, req.Dir, req.Message)
	}
	lane := m.lanes[crew.crewKey()]
	if lane == nil || lane.crewGroup != "g" || lane.running {
		t.Fatalf("lane %+v", lane)
	}
	if len(a.pending) != 0 || !slices.Contains(a.allow, agent.ClaudeRule("Bash", "npm test")) {
		t.Errorf("a should have its approval: pending %+v", a.pending)
	}
	if !slices.ContainsFunc(crew.entries, func(e chatEntry) bool {
		return e.text == "Answered 1 approval while busy: approved `npm test` for feature/a"
	}) {
		t.Errorf("owner's chat: %+v", crew.entries)
	}
	if !slices.ContainsFunc(m.stateOf(crew).reports, func(r string) bool { return strings.Contains(r, "feature/c") }) {
		t.Errorf("the out-of-scope decision should reach the owner: %q", m.stateOf(crew).reports)
	}
	for f, b := range m.changedSessions() {
		if strings.Contains(string(b), lane.uid) {
			t.Errorf("the lane isn't saved, but %s has it", f)
		}
	}
	if lane.turnAttr.Plan != p.ID || lane.turnAttr.Group != "g" || lane.turnAttr.Kind != usage.KindDecide {
		t.Errorf("lane turn counted as %+v", lane.turnAttr)
	}
	if crew.running != true {
		t.Error("the owner's own turn goes on")
	}
}
