package ui

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/azeem-yousaf/grove/internal/agent"
)

func TestRetryDelay(t *testing.T) {
	for fails, want := range map[int]time.Duration{1: 10 * time.Second, 2: 20 * time.Second, 3: 40 * time.Second, 6: 5 * time.Minute, 20: 5 * time.Minute} {
		if got := retryDelay(fails); got != want {
			t.Errorf("retryDelay(%d) = %v, want %v", fails, got, want)
		}
	}
}

// A failed planner turn leaves nothing undecided: what it was asked waits
// for a later try, which backs off, and the user hears after three.
func TestFailedPlannerTurnIsRetried(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.termFocused = false // so alerts are kept
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m.now = t0
	fail := []agent.Event{{Kind: agent.EvDone, Err: errors.New("overloaded")}}
	fake.turns = [][]agent.Event{fail}
	c := m.sessionFor(m.taskPath(p, "feature/c")) // ungrouped: the lead's
	c.running = true
	pump(t, m, m.onAgentDone(c, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: "npm test"}}}))
	st := m.stateOf(m.planner)
	if len(fake.reqs) != 1 || len(c.pending) != 1 || c.pending[0].asked != 0 || st.fails != 1 || len(m.orch.queue) != 1 {
		t.Fatalf("after one failure: %d requests, pending %+v, fails %d, queue %+v", len(fake.reqs), c.pending, st.fails, m.orch.queue)
	}
	if !st.retryAt.Equal(t0.Add(retryFirst)) || st.turns != 0 {
		t.Errorf("retry at %v, turns %d", st.retryAt, st.turns)
	}

	// It waits before trying again.
	m.now = t0.Add(5 * time.Second)
	pump(t, m, m.orchestrate())
	if len(fake.reqs) != 1 {
		t.Fatal("it should wait before retrying")
	}
	fake.turns = [][]agent.Event{fail, fail}
	m.now = t0.Add(retryFirst)
	pump(t, m, m.orchestrate())
	m.now = m.now.Add(retryDelay(2))
	pump(t, m, m.orchestrate())
	if len(fake.reqs) != 3 || st.fails != 3 || !st.failAlerted || !slices.Contains(m.alerts, "The planner keeps failing: overloaded") {
		t.Fatalf("after three failures: %d requests, fails %d, alerts %q", len(fake.reqs), st.fails, m.alerts)
	}
	if len(c.pending) != 1 || c.pending[0].asked != 0 {
		t.Fatalf("the approval must still wait for the planner: %+v", c.pending)
	}

	// Once it works, the approval is answered and the count starts over.
	fake.turns = [][]agent.Event{plannerSays(`{"approve": [{"branch": "feature/c", "id": "` + c.pending[0].id + `"}]}`)}
	m.now = m.now.Add(retryDelay(3))
	pump(t, m, m.orchestrate())
	if len(c.pending) != 0 || st.fails != 0 || st.failAlerted || !st.retryAt.IsZero() {
		t.Errorf("after success: pending %+v, fails %d", c.pending, st.fails)
	}
}

// Stopping the planner isn't a failure to alert about.
func TestStoppedPlannerTurnIsNotAFailure(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	m.now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c := m.sessionFor(m.taskPath(p, "feature/c"))
	fake.turns = [][]agent.Event{{{Kind: agent.EvDone, Err: errors.New("stopped")}}}
	pl := m.plannerFor(p.ID, "")
	pl.stopping = true // as ctrl+x does while the turn runs
	c.running = true
	pump(t, m, m.onAgentDone(c, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: "npm test"}}}))
	st := m.stateOf(pl)
	if len(fake.reqs) != 1 || st.fails != 0 || len(c.pending) != 1 || len(m.orch.queue) != 1 || !strings.Contains(fake.reqs[0].Message, "npm test") {
		t.Errorf("requests %d, fails %d, pending %+v, queue %+v", len(fake.reqs), st.fails, c.pending, m.orch.queue)
	}
}
