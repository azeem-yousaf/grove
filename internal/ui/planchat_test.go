package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
)

func TestPlanChatWakesThePlannerNotTheAgents(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	log, _ := m.groupLog(p, plan.ChatID)
	read := func() {
		m.pollingGroups = false
		_, cmd := m.Update(m.pollGroups()())
		pump(t, m, cmd)
	}
	read() // nothing yet; later messages are news

	// The user's message to everyone goes to the planner, whose answer
	// comes back in the plan chat; no task agent is woken for it.
	fake.turns = [][]agent.Event{plannerSays(`{"post": [{"plan": "p1", "group": "plan", "message": "I'll ask feature/c to fix it."}]}`)}
	if err := log.Post(group.Message{From: group.User, Text: "The checkout button is the wrong colour."}); err != nil {
		t.Fatal(err)
	}
	read()
	if len(fake.reqs) != 1 || !fake.reqs[0].ReadOnly {
		t.Fatalf("only the planner should be woken: %+v", fake.reqs)
	}
	if ask := fake.reqs[0].Message; !strings.Contains(ask, "plan chat") || !strings.Contains(ask, "wrong colour") {
		t.Errorf("planner request:\n%s", ask)
	}
	msgs, _ := log.All()
	if last := msgs[len(msgs)-1]; last.From != group.Planner || last.Text != "I'll ask feature/c to fix it." {
		t.Errorf("last message = %+v", last)
	}

	// A message addressed to one agent wakes that agent.
	if err := log.Post(group.Message{From: group.Planner, To: "feature/c", Text: "Use the brand colour."}); err != nil {
		t.Fatal(err)
	}
	read()
	turns := taskTurns(fake)
	if len(turns) != 1 || turns[0].Dir != m.taskPath(p, "feature/c") || !strings.Contains(turns[0].Message, "plan chat") {
		t.Fatalf("feature/c should be woken: %+v", turns)
	}
}

func TestPlanChatThroughTheControlTools(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	dir := control.Dir(m.store.Dir())
	m.pollingControl = false
	_, cmd := m.Update(m.pollControl()())
	pump(t, m, cmd)
	st, err := control.ReadState(dir)
	if err != nil || len(st.Plans) != 1 || st.Plans[0].ID != "p1" || len(st.Plans[0].Groups) != 1 {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if err := control.Queue(dir, control.Order{Action: control.Post, Plan: "p1", Text: "Merged; have a look."}); err != nil {
		t.Fatal(err)
	}
	m.pollingControl = false
	msg := m.pollControl()()
	_, cmd = m.Update(msg)
	pump(t, m, cmd)
	log, _ := m.groupLog(p, plan.ChatID)
	msgs, _ := log.All()
	if len(msgs) != 1 || msgs[0].From != group.Planner || msgs[0].Text != "Merged; have a look." {
		t.Errorf("plan chat = %+v", msgs)
	}
}

func TestOpenPlanChat(t *testing.T) {
	m, p, fake := groupedPlan(t)
	open := m.openGroupChat(m.taskPath(p, "feature/c"))
	drive(t, m).until(open, func() bool { return !m.pollingGroups })
	if m.focus != focusGroup || m.groupChat.group != plan.ChatID {
		t.Fatalf("an isolated task's t opens the plan chat: %+v", m.groupChat)
	}
	out := plain(m)
	if !strings.Contains(out, "Plan chat · Shop") || !strings.Contains(out, "The planner posts here") || !strings.Contains(out, "feature/c") {
		t.Errorf("plan chat:\n%s", out)
	}

	// From a group's chat, ctrl+t goes to the plan chat and back.
	open = m.openGroupChat(m.taskPath(p, "feature/a"))
	drive(t, m).until(open, func() bool { return !m.pollingGroups })
	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if m.groupChat.group != plan.ChatID || m.groupChat.back != "g" {
		t.Fatalf("ctrl+t should open the plan chat: %+v", m.groupChat)
	}
	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if m.groupChat.group != "g" {
		t.Fatalf("ctrl+t again goes back: %+v", m.groupChat)
	}

	// Posting there with @branch addresses one agent.
	m.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	typeText(m, "@feature/c try again")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	runCmds(cmd)
	log, _ := m.groupLog(p, plan.ChatID)
	msgs, _ := log.All()
	if len(msgs) != 1 || msgs[0].To != "feature/c" || msgs[0].Text != "try again" {
		t.Errorf("posted %+v", msgs)
	}
	_ = fake
}
