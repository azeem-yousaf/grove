package ui

import (
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// taskTurns are the turns of task agents, not the planner's.
func taskTurns(f *fakeAgent) []agent.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []agent.Request
	for _, r := range f.reqs {
		if !r.ReadOnly {
			out = append(out, r)
		}
	}
	return out
}

func TestWatchingPlannerGetsCommandsAndTools(t *testing.T) {
	m, p, _ := groupedPlan(t)
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	if req := m.agentRequest(m.planner); req.Commands || len(req.Tools) != 0 {
		t.Fatalf("a plan the user watches gives the planner nothing more: %+v", req)
	}
	p.Orchestrated = true
	req := m.agentRequest(m.planner)
	if !req.ReadOnly || !req.Commands || len(req.Tools) != 1 || strings.Join(req.Tools[0].Args, " ") != "mcp-plan --dir "+control.Dir(m.store.Dir())+" --as lead" {
		t.Fatalf("while it watches a running plan the planner may run commands and has its tools: %+v", req)
	}
	if a := m.agentRequest(m.sessionFor(m.taskPath(p, "feature/a"))); a.Commands {
		t.Error("task agents still ask before running commands")
	}
	p.Status = plan.Done
	if req := m.agentRequest(m.planner); req.Commands || len(req.Tools) != 0 {
		t.Errorf("once the plan is done the planner only reads again: %+v", req)
	}
}

func TestControlOrdersAreCarriedOut(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	dir := control.Dir(m.store.Dir())

	// grove publishes the watched tasks, with what each agent waits on.
	a := m.sessionFor(m.taskPath(p, "feature/a"))
	a.pending = []pendingAction{{kind: pendingApproval, text: "npm test", tool: "Bash"}, {kind: pendingApproval, text: "git push", tool: "Bash"}}
	b := m.sessionFor(m.taskPath(p, "feature/b"))
	b.entries = []chatEntry{{roleAgent, "All done."}}
	for i := range p.Tasks {
		if p.Tasks[i].Branch == "feature/b" {
			p.Tasks[i].Status = plan.Finished
		}
	}
	m.Update(m.pollControl()())
	st, err := control.ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	ta, _ := st.Find("feature/a")
	tb, _ := st.Find("feature/b")
	if len(st.Plans) != 1 || len(ta.Approvals) != 2 || ta.Approvals[0].Command != "npm test" || tb.LastReply != "All done." || tb.Status != "finished" {
		t.Fatalf("state = %+v", st)
	}

	for _, o := range []control.Order{
		{By: control.Lead, Action: control.Approve, Task: "feature/a", Text: "npm test"},
		{By: control.Lead, Action: control.Deny, Task: "feature/a", Text: "git push", Note: "the user pushes"},
		{By: control.Lead, Action: control.Message, Task: "feature/b", Text: "Add a test too."},
		{By: control.Lead, Action: control.Fail, Task: "feature/c", Note: "not needed"},
	} {
		if err := control.Queue(dir, o); err != nil {
			t.Fatal(err)
		}
	}
	settle(t, m, m.pollControl(), func() bool { return len(taskTurns(fake)) == 2 && !a.running && !b.running && !m.planner.running })
	if len(a.pending) != 0 || !strings.Contains(strings.Join(a.allow, " "), "npm test") || strings.Contains(strings.Join(a.allow, " "), "git push") {
		t.Errorf("a: pending %v, allow %v", a.pending, a.allow)
	}
	var toA, toB string
	for _, r := range taskTurns(fake) {
		switch r.Dir {
		case a.path:
			toA = r.Message
		case b.path:
			toB = r.Message
		}
	}
	if !strings.Contains(toA, "Approved: `npm test`") || !strings.Contains(toA, "Denied: `git push` (the user pushes)") {
		t.Errorf("a was told %q", toA)
	}
	if !strings.Contains(toB, "Add a test too.") {
		t.Errorf("b was told %q", toB)
	}
	if c, _ := p.Task("feature/c"); c.Status != plan.Failed || c.Error != "not needed" {
		t.Errorf("c = %+v", c)
	}
	if orders, _ := control.Take(dir); len(orders) != 0 {
		t.Errorf("orders left over: %v", orders)
	}
}

func TestPlannerChecksFinishedTasks(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	s.running = true
	s.lastReply.WriteString("Done: added the endpoint.")
	pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone}))
	if len(fake.reqs) != 1 {
		t.Fatalf("want one planner turn, got %d", len(fake.reqs))
	}
	ask := fake.reqs[0]
	for _, want := range []string{"feature/a (task \"Work on feature/a\") says it is done", "> Done: added the endpoint.", "Check its work", "grove's tools"} {
		if !strings.Contains(ask.Message, want) {
			t.Errorf("planner request is missing %q:\n%s", want, ask.Message)
		}
	}
	if !ask.Commands {
		t.Error("the planner may run commands to check the work")
	}
}
