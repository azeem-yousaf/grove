package ui

import (
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// plannerTurns are the planners' turns, not the task agents'.
func plannerTurns(f *fakeAgent) []agent.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []agent.Request
	for _, r := range f.reqs {
		if r.ReadOnly {
			out = append(out, r)
		}
	}
	return out
}

// checkDone runs cmd's commands until one reports a task check.
func checkDone(t *testing.T, cmd tea.Cmd) checkDoneMsg {
	t.Helper()
	var find func(tea.Cmd) (checkDoneMsg, bool)
	find = func(c tea.Cmd) (checkDoneMsg, bool) {
		if c == nil {
			return checkDoneMsg{}, false
		}
		switch msg := c().(type) {
		case checkDoneMsg:
			return msg, true
		case tea.BatchMsg:
			for _, c := range msg {
				if d, ok := find(c); ok {
					return d, true
				}
			}
		}
		return checkDoneMsg{}, false
	}
	d, ok := find(cmd)
	if !ok {
		t.Fatal("no task check ran")
	}
	return d
}

func slowCommand() string {
	if runtime.GOOS == "windows" {
		return "ping -n 20 127.0.0.1"
	}
	return "sleep 20"
}

func TestFinishedTaskIsTestedFirst(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.cfg.Planner.Test = "echo boom && exit 1"
	s := m.sessionFor(m.taskPath(p, "feature/c"))
	s.running = true
	s.lastReply.WriteString("Done: added the endpoint.")
	cmd := m.onAgentDone(s, agent.Event{Kind: agent.EvDone})
	// grove reads its HEAD first, then starts the check.
	d := drive(t, m)
	d.until(cmd, func() bool { return m.checkIn(s.path) != nil })
	if m.planner != nil && m.planner.running || slices.ContainsFunc(m.orch.queue, func(it orchItem) bool { return it.kind == orchFinished }) {
		t.Fatalf("the planner hears about finished work only once its tests ran: queue %+v", m.orch.queue)
	}
	if !slices.ContainsFunc(m.attentionItems(), func(a attention) bool { return strings.Contains(a.text, "grove is running echo boom") }) {
		t.Errorf("the overview should show the check: %+v", m.attentionItems())
	}
	d.until(nil, func() bool { return len(plannerTurns(fake)) == 1 })
	ask := plannerTurns(fake)[0].Message
	for _, want := range []string{"feature/c (task \"Work on feature/c\") says it is done", "> Done: added the endpoint.",
		"grove ran `echo boom && exit 1` in its worktree and it failed with exit code 1", "boom", "grove has already run the tests", "test tool"} {
		if !strings.Contains(ask, want) {
			t.Errorf("planner request is missing %q:\n%s", want, ask)
		}
	}
	if len(m.checks) != 0 {
		t.Errorf("checks left: %v", m.checks)
	}
}

func TestPassingCheckSaysSo(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.cfg.Planner.Test = "exit 0"
	s := m.sessionFor(m.taskPath(p, "feature/c"))
	s.running = true
	s.lastReply.WriteString("Done.")
	settle(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone}), func() bool { return len(plannerTurns(fake)) == 1 })
	if ask := plannerTurns(fake)[0].Message; !strings.Contains(ask, "grove ran `exit 0` in its worktree and it passed.") {
		t.Errorf("planner request:\n%s", ask)
	}
}

func TestReopeningCancelsTheCheck(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	m.cfg.Planner.Test = slowCommand()
	c, _ := p.Task("feature/c")
	c.Status = plan.Finished
	cmd := m.testFinished(p, c, "Done.", "")
	if cmd == nil || m.checkIn(c.Path) == nil {
		t.Fatal("no check started")
	}
	m.decide(nil, decisions{Reply: []replyDecision{{Branch: "feature/c", Message: "Add a test too."}}})
	if len(m.checks) != 0 {
		t.Fatal("reopening the task should cancel its check")
	}
	start := time.Now()
	msg := checkDone(t, cmd)
	if time.Since(start) > 10*time.Second {
		t.Error("the command wasn't stopped")
	}
	m.Update(msg)
	if slices.ContainsFunc(m.orch.queue, func(it orchItem) bool { return it.kind == orchFinished }) {
		t.Errorf("a cancelled check's result is dropped: %+v", m.orch.queue)
	}
}

func TestNoTestCommandQueuesAtOnce(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	c, _ := p.Task("feature/c")
	c.Status = plan.Finished
	if cmd := m.testFinished(p, c, "Done.", ""); cmd != nil || len(m.checks) != 0 || len(m.orch.queue) != 1 || m.orch.queue[0].kind != orchFinished {
		t.Errorf("checks %v, queue %+v", m.checks, m.orch.queue)
	}
}

func TestTestToolReportsToThePlannerThatAsked(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	dir := control.Dir(m.store.Dir())
	m.Update(m.pollControl()())
	m.plannerFor(p.ID, "g") // the group's planner, whose tool server sent o-1 and o-2
	crew := control.Caller{Plan: p.ID, Group: "g"}.String()
	for _, o := range []control.Order{
		{ID: "o-1", Action: control.Test, Task: "feature/a", Text: "echo checked", By: crew},
		{ID: "o-2", Action: control.Test, Task: "feature/c", Text: "exit 0", By: crew},
		{ID: "o-3", Action: control.Test, Task: "feature/c", Text: "git push origin feature/c", By: control.Lead},
		{ID: "o-4", Action: control.Test, Task: "feature/b", By: control.Lead},
	} {
		if err := control.Queue(dir, o); err != nil {
			t.Fatal(err)
		}
	}
	settle(t, m, m.pollControl(), func() bool {
		_, ok := control.ResultOf(dir, "o-4")
		return ok && slices.ContainsFunc(plannerTurns(fake), func(r agent.Request) bool { return r.Slot != "" })
	})
	for id, want := range map[string]string{
		"o-1": control.Applied + ": started `echo checked`",
		"o-2": control.NotFound + ": not a task of your group",
		"o-3": control.Failed + ": test doesn't run git",
		"o-4": control.Failed + ": the plan has no test command",
	} {
		r, _ := control.ResultOf(dir, id)
		if got := r.Outcome + ": " + r.Detail; !strings.HasPrefix(got, want) {
			t.Errorf("%s = %q, want %q", id, got, want)
		}
	}
	var ask string
	for _, r := range plannerTurns(fake) {
		if r.Slot != "" {
			ask = r.Message
		} else {
			t.Errorf("the lead didn't ask for the check: %s", r.Message)
		}
	}
	if !strings.Contains(ask, "The test run you asked for in feature/a's worktree has ended: grove ran `echo checked` in its worktree and it passed.") {
		t.Errorf("group planner's request:\n%s", ask)
	}
	if len(m.checks) != 0 {
		t.Errorf("checks left: %v", m.checks)
	}
}
