package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// Each planner posts under its own name, hears the others, and hears agents
// that address it.
func TestPlannersHearEachOther(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.cfg.Planner.Settle = nil // the planner waits 5 s, so the queue stays
	m.now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, b := range []string{"feature/a", "feature/b", "feature/c"} {
		m.sessionFor(m.taskPath(p, b)).running = true
	}
	read := func() {
		m.pollingGroups = false
		_, cmd := m.Update(m.pollGroups()())
		pump(t, m, cmd)
	}
	read() // history
	crewLog, _ := m.groupLog(p, "g")
	planLog, _ := m.groupLog(p, plan.ChatID)
	crewLog.Post(group.Message{From: group.PlannerOf("g"), Text: "my own post"})
	crewLog.Post(group.Message{From: "feature/a", To: group.Planner, Text: "Which port do we use"})
	planLog.Post(group.Message{From: group.PlannerOf("g"), Text: "The checkout API is ready."})
	read()
	if len(fake.reqs) != 0 {
		t.Fatalf("nothing should have been sent yet: %d requests", len(fake.reqs))
	}
	find := func(groupID string) *orchItem {
		i := slices.IndexFunc(m.orch.queue, func(it orchItem) bool { return it.kind == orchGroup && it.group == groupID })
		if i < 0 {
			t.Fatalf("no item for %s in %+v", groupID, m.orch.queue)
		}
		return &m.orch.queue[i]
	}
	g := find("g")
	if !g.urgent || len(g.lines) != 1 || g.lines[0] != "feature/a → planner: Which port do we use" {
		t.Errorf("the group's planner should be asked, without its own post: %+v", g)
	}
	if m.ownerOf(*g) != "g" {
		t.Error("the group's planner owns its chat")
	}
	lead := find(plan.ChatID)
	if !lead.urgent || len(lead.lines) != 1 || !strings.HasPrefix(lead.lines[0], "planner:g: ") {
		t.Errorf("the lead should hear the group's planner: %+v", lead)
	}

	// Members are told who the planner is.
	if info := groupInfo(p, plan.Group{ID: "g", Name: "Checkout"}); info.Planner != "planner:g" {
		t.Errorf("group info planner = %q", info.Planner)
	}
	if info := groupInfo(p, plan.Group{ID: plan.ChatID}); info.Planner != group.Planner {
		t.Errorf("plan chat info planner = %q", info.Planner)
	}
	if ctx := m.agentContext(m.sessionFor(m.taskPath(p, "feature/c"))); !strings.Contains(ctx, "post with `to` \"planner\"") {
		t.Errorf("agents should know they can address the planner:\n%s", ctx)
	}
}
