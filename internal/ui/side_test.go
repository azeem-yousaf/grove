package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
)

func TestSideTurnAnswersWhilePlannerIsBusy(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	lead := m.plannerFor(p.ID, "")
	lead.id, lead.running = "lead-1", true // running the test suite, say
	fake.turns = [][]agent.Event{plannerSays(`{"approve": [{"branch": "feature/c", "command": "npm test"}]}`)}
	c := m.sessionFor(m.taskPath(p, "feature/c"))
	c.running = true
	pump(t, m, m.onAgentDone(c, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: "npm test"}}}))

	if len(fake.reqs) < 2 {
		t.Fatalf("want a side turn and c resumed, got %d requests", len(fake.reqs))
	}
	side := fake.reqs[0]
	if !side.Fork || side.Session != "lead-1" || !side.ReadOnly || side.Slot != sideKey("") || !strings.Contains(side.Message, "`npm test`") || !strings.Contains(side.Message, sideAsk) {
		t.Errorf("side turn request: %+v", side)
	}
	if !lead.running || len(c.pending) != 0 || !slices.Contains(c.allow, agent.ClaudeRule("Bash", "npm test")) {
		t.Errorf("lead running %v, pending %+v, allow %v", lead.running, c.pending, c.allow)
	}
	if !slices.ContainsFunc(lead.entries, func(e chatEntry) bool { return strings.HasPrefix(e.text, "While you work, a side turn decides") }) {
		t.Errorf("the lead's chat should show the side turn: %+v", lead.entries)
	}
	if s := m.sides[sideKey("")]; s == nil || s.uid == lead.uid || s.turnAttr.Plan != p.ID || s.turns != 1 {
		t.Errorf("side session %+v", s)
	}

	// c finished after its approval; that waits for the lead's main turn.
	n := len(fake.reqs)
	if !slices.ContainsFunc(m.orch.queue, func(it orchItem) bool { return it.kind == orchFinished && it.branch == "feature/c" }) {
		t.Fatalf("the finished task should wait for the lead: %+v", m.orch.queue)
	}
	lead.running = false
	pump(t, m, m.onAgentDone(lead, agent.Event{Kind: agent.EvDone}))
	if len(fake.reqs) != n+1 {
		t.Fatalf("want the lead's turn, got %d requests after %d", len(fake.reqs), n)
	}
	main := fake.reqs[n]
	if main.Fork || main.Session != "lead-1" || !strings.Contains(main.Message, "feature/c (task \"Work on feature/c\") says it is done") ||
		!strings.Contains(main.Message, "While you were busy") || !strings.Contains(main.Message, "approved `npm test` for feature/c") {
		t.Errorf("lead's next request:\n%s", main.Message)
	}
	if len(m.orch.whileBusy) != 0 {
		t.Error("the report is given once")
	}

	// The lead approving the same command again changes nothing.
	_, outs := m.decide(lead, decisions{Approve: []answerDecision{{Branch: "feature/c", Command: "npm test"}}})
	if len(outs) != 1 || outs[0].result != control.AlreadyApplied {
		t.Errorf("approving again = %+v", outs)
	}
}

func TestNoSideTurnWithoutASession(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	lead := m.plannerFor(p.ID, "")
	lead.running = true // its first turn: nothing to fork yet
	c := m.sessionFor(m.taskPath(p, "feature/c"))
	c.running = true
	pump(t, m, m.onAgentDone(c, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: "npm test"}}}))
	if len(fake.reqs) != 0 || len(m.sides) != 0 || len(m.orch.queue) != 1 {
		t.Errorf("requests %d, sides %d, queue %+v", len(fake.reqs), len(m.sides), m.orch.queue)
	}

	// Once it has one, side turns count towards its decision limit.
	lead.id, m.cfg.Planner.MaxDecisions = "lead-1", 1
	m.orchestrate()
	side := m.sides[sideKey("")]
	if side == nil || !side.running || m.orch.turns != 1 || len(m.orch.queue) != 0 {
		t.Fatalf("side %v, turns %d, queue %+v", side != nil, m.orch.turns, m.orch.queue)
	}
	side.running = false
	m.queueTurn(c, false)
	m.orchestrate()
	if side.turns != 1 || len(m.orch.queue) != 1 {
		t.Errorf("past the limit: side turns %d, queue %+v", side.turns, m.orch.queue)
	}
}

func TestGroupPlannersSideTurnStaysInItsGroup(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	crew := m.plannerFor(p.ID, "g")
	crew.id, crew.running = "crew-1", true
	fake.turns = [][]agent.Event{plannerSays(`{"approve": [{"branch": "feature/a", "command": "npm test"}],
		"reply": [{"branch": "feature/c", "message": "blue"}], "merge": [{"plan": "p1"}]}`)}
	a, c := m.sessionFor(m.taskPath(p, "feature/a")), m.sessionFor(m.taskPath(p, "feature/c"))
	a.running = true
	pump(t, m, m.onAgentDone(a, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: "npm test"}}}))

	if len(fake.reqs) == 0 {
		t.Fatal("no side turn")
	}
	req := fake.reqs[0]
	key := sideKey(crewKey(p.ID, "g"))
	if !req.Fork || req.Session != "crew-1" || req.Slot != key || !strings.Contains(req.Message, "in charge of your group") {
		t.Errorf("side turn request: %+v", req)
	}
	if m.planner != nil {
		t.Error("the lead isn't involved")
	}
	if !slices.Contains(a.allow, agent.ClaudeRule("Bash", "npm test")) {
		t.Errorf("a's allow = %v", a.allow)
	}
	if slices.ContainsFunc(fake.reqs, func(r agent.Request) bool { return r.Dir == c.path }) {
		t.Error("feature/c isn't in the group")
	}
	notes := func(text string) bool {
		return slices.ContainsFunc(crew.entries, func(e chatEntry) bool { return strings.Contains(e.text, text) })
	}
	if !notes("Not applied: reply to feature/c") || !notes("Not applied: merge p1") {
		t.Errorf("the group planner's chat should note what didn't apply: %+v", crew.entries)
	}
	if b := crew.crew.whileBusy; len(b) != 1 || b[0] != "approved `npm test` for feature/a" {
		t.Errorf("whileBusy = %q", b)
	}

	// Saved apart from the group's planner, without a chat.
	side := m.sides[key]
	if side == nil || side.turnAttr.Group != "g" || strings.HasPrefix(side.file(), crewDir+"/") {
		t.Fatalf("side session %+v", side)
	}
	if r := restore(side.saved(), true, true); !r.side || r.slot() != key || len(r.entries) != 0 {
		t.Errorf("restored %+v", r)
	}
}
