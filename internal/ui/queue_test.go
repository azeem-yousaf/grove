package ui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/plan"
)

func typeIn(m *Model, text string) {
	for _, r := range text {
		press(m, string(r))
	}
	press(m, "enter")
}

// What the user types while the agent works waits for the end of its turn.
func TestMessagesQueueWhileTheAgentWorks(t *testing.T) {
	m, p, fake := groupedPlan(t)
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	m.showChat(s)
	s.running = true
	typeIn(m, "hi")
	typeIn(m, "also this")
	if !slices.Equal(s.queued, []string{"hi", "also this"}) || m.chatInput.Value() != "" || len(fake.reqs) != 0 {
		t.Fatalf("queued %q, input %q, %d requests", s.queued, m.chatInput.Value(), len(fake.reqs))
	}
	if out := plain(m); !strings.Contains(out, "queued (2): also this") || !strings.Contains(out, "ctrl+u") {
		t.Errorf("the chat should show the queued messages:\n%s", out)
	}
	if r := restore(s.saved(), true, true); !slices.Equal(r.queued, s.queued) {
		t.Errorf("restored queue %q", r.queued)
	}
	pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone}))
	if len(fake.reqs) == 0 || fake.reqs[0].Message != "hi\n\nalso this" || len(s.queued) != 0 {
		t.Fatalf("the queued messages should go as one: %+v", fake.reqs)
	}
	if _, task := m.taskFor(s.path); task.Status != plan.Finished {
		t.Errorf("the task finishes after the queued turn, not before: %v", task.Status)
	}

	// ctrl+u drops what is queued.
	s.running = true
	typeIn(m, "never mind")
	m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	if len(s.queued) != 0 {
		t.Errorf("ctrl+u should drop the queue: %q", s.queued)
	}
}

// With a card waiting, the queued message goes with the answer.
func TestQueuedMessageGoesWithTheAnswers(t *testing.T) {
	m, p, fake := groupedPlan(t)
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	m.showChat(s)
	s.running = true
	typeIn(m, "use port 8080")
	pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: "npm test"}}}))
	if len(fake.reqs) != 0 || len(s.queued) != 1 {
		t.Fatalf("it waits for the card: %d requests, queue %q", len(fake.reqs), s.queued)
	}
	m.focus = focusChat
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)
	if len(fake.reqs) == 0 || !strings.Contains(fake.reqs[0].Message, "I approved `npm test`") || !strings.Contains(fake.reqs[0].Message, "The user also said:\n\nuse port 8080") {
		t.Fatalf("requests %+v", fake.reqs)
	}
}

// A message queued for the planner goes before grove's next request.
func TestQueuedMessageBeatsOrchestration(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	pl := m.plannerFor(p.ID, "")
	pl.running = true
	pl.queued = []string{"Hold off merging."}
	m.orch.queue = append(m.orch.queue, orchItem{kind: orchGroup, plan: p.ID, group: plan.ChatID, lines: []string{"feature/c: done"}, urgent: true, now: true})
	pump(t, m, m.onAgentDone(pl, agent.Event{Kind: agent.EvDone}))
	if len(fake.reqs) < 2 || fake.reqs[0].Message != "Hold off merging." || !strings.Contains(fake.reqs[1].Message, "New messages in the plan chat") {
		var got []string
		for _, r := range fake.reqs {
			got = append(got, r.Message)
		}
		t.Fatalf("requests: %q", got)
	}
}
