package ui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/agenthook"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/usage"
)

// A group's planner's control tools know who they work for, and grove
// carries out its orders within its group: an approval of another task is
// refused even by ID and even when the order names a task of its group.
func TestCrewControlOrdersStayInTheGroup(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	g := &grants{}
	m.grant = g.add
	crew := m.plannerFor(p.ID, "g")
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	if req := m.agentRequest(crew); len(req.Tools) == 0 || !slices.Contains(req.Tools[len(req.Tools)-1].Args, "crew:p1:g") {
		t.Fatalf("crew's tools = %+v", req.Tools)
	}
	if req := m.agentRequest(m.planner); len(req.Tools) != 1 || !slices.Contains(req.Tools[0].Args, control.Lead) {
		t.Fatalf("lead's tools = %+v", req.Tools)
	}

	a, c := m.sessionFor(m.taskPath(p, "feature/a")), m.sessionFor(m.taskPath(p, "feature/c"))
	aa, ac := m.newApproval(a, "Bash", "npm test"), m.newApproval(c, "Bash", "make")
	a.pending, c.pending = []pendingAction{aa}, []pendingAction{ac}
	by := callerOf(crew).String()
	orders := []control.Order{
		{ID: "o-1", By: by, Action: control.Approve, Task: "feature/a", Item: ac.id, Text: "make"},
		{ID: "o-2", By: by, Action: control.Deny, Item: ac.id},
		{ID: "o-3", By: by, Action: control.Message, Task: "feature/c", Text: "hi"},
		{ID: "o-4", By: by, Action: control.Merge, Plan: p.ID},
		{ID: "o-5", Action: control.Approve, Task: "feature/c", Item: ac.id},
		{ID: "o-6", By: "crew:p1:nope", Action: control.Approve, Task: "feature/c", Item: ac.id},
		{ID: "o-7", By: by, Action: control.Approve, Task: "feature/a", Item: aa.id},
	}
	pump(t, m, m.onControl(controlMsg{orders: orders}))
	dir := control.Dir(m.store.Dir())
	for id, want := range map[string]string{"o-1": control.NotFound, "o-2": control.NotFound, "o-3": control.NotFound, "o-4": control.Failed,
		"o-5": control.Failed, "o-6": control.Failed, "o-7": control.Applied} {
		if r, ok := control.ResultOf(dir, id); !ok || r.Outcome != want {
			t.Errorf("result of %s = %+v, want %s", id, r, want)
		}
	}
	if r, _ := control.ResultOf(dir, "o-1"); !strings.Contains(r.Detail, "not a task of your group") {
		t.Errorf("o-1 = %+v", r)
	}
	if len(c.pending) != 1 || len(c.answered) != 0 {
		t.Errorf("c's approval should still wait: %+v %+v", c.pending, c.answered)
	}
	if len(a.answered) != 1 || !a.answered[0].Approve {
		t.Errorf("a's approval should be answered: %+v", a.answered)
	}

	// The same holds for a grove-orchestrate block, and the lead reaches
	// every task.
	d, outs := m.scopeDecisions(crew, decisions{Approve: []answerDecision{{ID: ac.id, Branch: "feature/b"}}})
	if len(d.Approve) != 0 || len(outs) != 1 || outs[0].detail != "not a task of your group" {
		t.Errorf("crew block: %+v %+v", d, outs)
	}
	if d, outs := m.scopeDecisions(m.planner, decisions{Approve: []answerDecision{{ID: ac.id}}}); len(d.Approve) != 1 || len(outs) != 0 {
		t.Errorf("lead block: %+v %+v", d, outs)
	}
}

// Each group's planner has a git scope and grants of its own: it may run
// git only in its group's worktrees, and what it is granted isn't the
// lead's.
func TestCrewGitScope(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	m.planner = m.newPlanner(m.plannerDir(), agent.Claude)
	crew := m.plannerFor(p.ID, "g")
	useFakeGuard(t, m, nil)
	dir, _ := m.controlDir()
	as := m.crewAs(p.ID, "g")
	if as == m.plannerAs() || as == "" {
		t.Fatalf("crew hook identity %q", as)
	}
	if req := m.agentRequest(crew); req.Hook[len(req.Hook)-1] != as {
		t.Errorf("crew hook = %v", req.Hook)
	}
	m.auto.policyJSON = nil
	m.writePolicy()
	pol, err := agenthook.ReadPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}
	aPath, bPath, cPath := m.taskPath(p, "feature/a"), m.taskPath(p, "feature/b"), m.taskPath(p, "feature/c")
	sc, ok := pol.Scope(as)
	if !ok || !sc.Planner || !slices.Equal(sc.Writable, []string{aPath, bPath}) || !slices.Contains(sc.Readable, cPath) {
		t.Errorf("crew's scope = %+v", sc)
	}
	if lead, ok := pol.Scope(m.plannerAs()); !ok || !slices.Contains(lead.Writable, cPath) || !slices.Contains(lead.Writable, aPath) {
		t.Errorf("lead's scope = %+v", lead)
	}
	if err := m.gitGrant(crew, "Bash", "git status", approvalPolicy{}, byPlanner); err != nil {
		t.Fatal(err)
	}
	if _, ok := agenthook.TakeGrant(dir, m.plannerAs(), "git status"); ok {
		t.Error("the lead used the crew's grant")
	}
	if _, ok := agenthook.TakeGrant(dir, as, "git status"); !ok {
		t.Error("the crew's grant should be under its own identity")
	}
}

// A closed plan's group planners go: their running turn is recorded,
// their process stopped and their saved conversation deleted; one restored
// for a closed plan is dropped as well.
func TestClosedPlansDropTheirCrews(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated, p.Status = true, plan.Review
	withLedger(t, m)
	var mu sync.Mutex
	var stopped []string
	m.stopAgent = func(key string) {
		mu.Lock()
		defer mu.Unlock()
		stopped = append(stopped, key)
	}
	crew := m.plannerFor(p.ID, "g")
	m.startTurn(crew)
	crew.running = true
	crew.live, crew.liveSeen = usage.Tokens{Input: 10, Output: 2}, true
	m.persistNow()
	file := filepath.Join(m.store.Dir(), filepath.FromSlash(crew.file()))
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("crew wasn't saved: %v", err)
	}
	saved := crew.saved()
	m.showChat(crew)
	side := &session{side: true, planner: true, uid: newUID(), path: crew.path, crewPlan: crew.crewPlan, crewGroup: crew.crewGroup}
	m.sides[side.slot()] = side

	pump(t, m, m.closePlan(p, "good"))
	if len(m.crews) != 0 {
		t.Errorf("crews left: %v", m.crews)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("crew's file is still there: %v", err)
	}
	mu.Lock()
	if want := agent.ReadOnlyKey(crew.path, crew.crewKey()); !slices.Equal(stopped, []string{want}) {
		t.Errorf("stopped %q, want %q", stopped, want)
	}
	if len(m.sides) != 0 {
		t.Errorf("side turns left: %v", m.sides)
	}
	mu.Unlock()
	if sum := m.usage.Agent(crew.uid); sum.Turns != 1 || sum.Tokens.Total() != 12 {
		t.Errorf("crew's usage = %+v", sum)
	}
	if m.chatSession() == crew || m.focus == focusChat {
		t.Error("the chat still shows the dropped planner")
	}

	// Restored after the plan was closed: dropped once the plans are known.
	m.plansLoaded = true
	r := restore(saved, true, true)
	pump(t, m, m.onSessionsLoaded(sessionsLoadedMsg{sessions: []*session{r}}))
	if len(m.crews) != 0 {
		t.Errorf("a closed plan's crew was restored: %v", m.crews)
	}
	// A running plan's is kept.
	p.Status = plan.Running
	r = restore(saved, true, true)
	pump(t, m, m.onSessionsLoaded(sessionsLoadedMsg{sessions: []*session{r}}))
	if len(m.crews) != 1 {
		t.Errorf("a running plan's crew should be restored: %v", m.crews)
	}
}

// A closed plan's branch may be reused by a later plan; only the later one
// is watched for it.
func TestDonePlansDontShadowBranches(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	old := &plan.Plan{ID: "p0", Title: "Old", Status: plan.Done, Orchestrated: true,
		Tasks: []plan.Task{{Branch: "feature/a", Path: m.taskPath(p, "feature/a"), Group: "x"}}}
	m.plans = []*plan.Plan{old, p}
	if got, task, _ := m.watchedTask("feature/a"); got != p || task == nil || task.Group != "g" {
		t.Errorf("watchedTask found plan %v", got)
	}
	p.Status = plan.Done
	if got, _, _ := m.watchedTask("feature/a"); got != nil {
		t.Errorf("a done plan's task was found: %v", got.ID)
	}
}
