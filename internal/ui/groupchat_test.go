package ui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// groupedPlan puts mergeModel's feature/a and feature/b in one group and
// feature/c on its own, as a started plan with an agent in each.
func groupedPlan(t *testing.T) (*Model, *plan.Plan, *fakeAgent) {
	t.Helper()
	m, _ := mergeModel(t)
	m.groveBin = "/usr/bin/grove"
	fake := &fakeAgent{}
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	m.runAgent = fake.run
	p := &plan.Plan{ID: "p1", Title: "Shop", Status: plan.Running, Agent: "claude",
		Groups: []plan.Group{{ID: "g", Name: "Checkout"}}}
	for _, st := range m.snap.States {
		task := plan.Task{ID: st.Branch, Title: "Work on " + st.Branch, Brief: "Do " + st.Branch, Branch: st.Branch, Path: st.Path, Status: plan.Working}
		switch st.Branch {
		case "feature/a", "feature/b":
			task.Group = "g"
		case "feature/c":
		default:
			continue
		}
		p.Tasks = append(p.Tasks, task)
		m.newSession(st.Path, st.Branch, agent.Claude)
	}
	m.plans = []*plan.Plan{p}
	return m, p, fake
}

func (m *Model) taskPath(p *plan.Plan, branch string) string {
	for _, t := range p.Tasks {
		if t.Branch == branch {
			return t.Path
		}
	}
	return ""
}

func TestGroupedAgentsGetTheirGroup(t *testing.T) {
	m, p, _ := groupedPlan(t)
	a := m.sessionFor(m.taskPath(p, "feature/a"))
	req := m.agentRequest(a)
	if len(req.Tools) != 1 || req.Tools[0].Command != "/usr/bin/grove" || strings.Join(req.Tools[0].Args, " ") != "mcp-group --dir "+group.Dir(m.store.Dir(), "p1", "g")+" --as feature/a" {
		t.Fatalf("grouped tools = %+v", req.Tools)
	}
	ctx := m.agentContext(a)
	if !strings.Contains(ctx, "## Your group: Checkout") || !strings.Contains(ctx, "- feature/b: Work on feature/b") || strings.Contains(ctx, "- feature/c:") {
		t.Errorf("grouped briefing:\n%s", ctx)
	}
	c := m.sessionFor(m.taskPath(p, "feature/c"))
	if req := m.agentRequest(c); len(req.Tools) != 0 || strings.Contains(m.agentContext(c), "Your group") {
		t.Error("an isolated task must not get the group")
	}

	// Saving the plan publishes the members for the members tool.
	for _, msg := range runCmds(m.savePlan(p)) {
		if pm, ok := msg.(persistMsg); ok && pm.err != nil {
			t.Fatal(pm.err)
		}
	}
	info, err := group.Log{Dir: group.Dir(m.store.Dir(), "p1", "g")}.ReadInfo()
	if err != nil || info.Name != "Checkout" || len(info.Members) != 2 || info.Members[1].Name != "feature/b" || info.Members[1].Path == "" {
		t.Errorf("members.json = %+v, %v", info, err)
	}
}

// poll reads the groups and handles what that starts, as the clock would.
func poll(t *testing.T, m *Model, fake *fakeAgent) {
	t.Helper()
	cmd := m.pollGroups()
	if cmd == nil {
		t.Fatal("nothing to poll")
	}
	_, next := m.Update(cmd())
	pump(t, m, next)
}

func TestIdleAgentsWakeForMessagesUpToTheLimit(t *testing.T) {
	m, p, fake := groupedPlan(t)
	m.cfg.Planner.MaxWakeups = 2
	log, _ := m.groupLog(p, "g")
	if err := log.Post(group.Message{From: "feature/a", Text: "The API returns {orderId}."}); err != nil {
		t.Fatal(err)
	}

	poll(t, m, fake)
	if len(fake.reqs) != 1 || !strings.Contains(fake.reqs[0].Dir, "shop-b") || !strings.Contains(fake.reqs[0].Message, "1 new message") {
		t.Fatalf("b should be woken once: %+v", fake.reqs)
	}
	// The fake never reads its messages, so each poll wakes it again, until
	// the limit; then grove waits for the user.
	poll(t, m, fake)
	poll(t, m, fake)
	if len(fake.reqs) != 2 {
		t.Fatalf("woken %d times, want the limit of 2", len(fake.reqs))
	}
	g := m.groups[log.Dir]
	if !g.paused || !strings.Contains(m.toast.text, "paused waking agents") {
		t.Errorf("paused %v, toast %q", g.paused, m.toast.text)
	}

	// The group chat shows it all; posting there resets the count.
	m.Update(tea.KeyPressMsg{Code: 't', Text: "t"}) // on main: not grouped
	if m.focus == focusGroup {
		t.Fatal("main isn't in a group")
	}
	open := m.openGroupChat(m.taskPath(p, "feature/a"))
	drive(t, m).until(open, func() bool { return !m.pollingGroups })
	if m.focus != focusGroup {
		t.Fatal("t should open the group chat")
	}
	out := plain(m)
	if !strings.Contains(out, "⇄ Checkout · Shop") || !strings.Contains(out, "The API returns {orderId}.") || !strings.Contains(out, "Paused") {
		t.Errorf("group chat:\n%s", out)
	}
	typeText(m, "@feature/b please add a test")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	var pollAgain tea.Cmd
	for _, msg := range runCmds(cmd) {
		if _, ok := msg.(groupPostedMsg); ok {
			_, pollAgain = m.Update(msg)
		}
	}
	msgs, _ := log.All()
	last := msgs[len(msgs)-1]
	if last.From != group.User || last.To != "feature/b" || last.Text != "please add a test" {
		t.Errorf("posted %+v", last)
	}
	if g := m.groups[log.Dir]; g.paused || g.wakeups != 0 {
		t.Errorf("posting should reset wake-ups: %+v", g)
	}
	if pollAgain == nil {
		t.Fatal("posting should read the group again")
	}
	_, next := m.Update(pollAgain())
	pump(t, m, next)
	if len(fake.reqs) != 3 || !strings.Contains(fake.reqs[2].Message, "2 new messages") {
		t.Errorf("after the user posts, b is woken again: %+v", fake.reqs)
	}
	// a has nothing addressed to it except its own post, so it slept.
	for _, r := range fake.reqs {
		if strings.Contains(r.Dir, "shop-a") {
			t.Error("a was woken for its own message")
		}
	}
}

func TestBusyAgentsAreNotWoken(t *testing.T) {
	m, p, fake := groupedPlan(t)
	log, _ := m.groupLog(p, "g")
	log.Post(group.Message{From: "feature/a", Text: "hello"})
	b := m.sessionFor(m.taskPath(p, "feature/b"))
	b.running = true
	cmd := m.pollGroups()
	m.Update(cmd())
	time.Sleep(50 * time.Millisecond)
	if len(fake.reqs) != 0 {
		t.Error("a working agent reads its messages itself; grove must not start another turn")
	}
}
