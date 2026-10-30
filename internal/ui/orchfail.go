package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
)

// A planner's turn that fails (or that the user stops) didn't decide
// anything, so what it was asked goes back in the queue rather than counting
// as left unanswered, and the next try waits: retryFirst after the first
// failure, doubling up to retryMax. After failAlert failures in a row the
// user hears about it, once until a turn succeeds.
const (
	retryFirst = 10 * time.Second
	retryMax   = 5 * time.Minute
	failAlert  = 3
)

// retryDelay is how long to wait after fails failures in a row.
func retryDelay(fails int) time.Duration {
	d := retryFirst
	for i := 1; i < fails && d < retryMax; i++ {
		d *= 2
	}
	return min(d, retryMax)
}

// plannerTurnEnded deals with what planner s was asked in the turn that
// just ended: on success what it left undecided (leaveUndecided); on failure
// it is queued again for a later turn.
func (m *Model) plannerTurnEnded(s *session, ev agent.Event, stopped bool) tea.Cmd {
	st := m.stateOf(s)
	sent := st.sent
	st.sent = nil
	if ev.Err == nil {
		st.fails, st.retryAt, st.failAlerted = 0, time.Time{}, false
		return m.leaveUndecided(s)
	}
	if len(sent) == 0 && len(st.asked) == 0 {
		return nil // a turn the user started; nothing waits on it
	}
	st.asked = nil
	st.turns = max(st.turns-1, 0) // it decided nothing
	m.orch.queue = append(m.orch.queue, sent...)
	if stopped {
		st.retryAt = m.now.Add(retryFirst)
		return nil
	}
	st.fails++
	st.retryAt = m.now.Add(retryDelay(st.fails))
	if st.fails < failAlert || st.failAlerted {
		return nil
	}
	st.failAlerted = true
	text := "The planner keeps failing: " + ev.Err.Error()
	if s.crewGroup != "" {
		text = "The planner of " + groupTitleOf(m.planByID(s.crewPlan), s.crewGroup) + " keeps failing: " + ev.Err.Error()
	}
	return tea.Batch(m.notify("✦ "+text, toastErr), m.alert(text))
}
