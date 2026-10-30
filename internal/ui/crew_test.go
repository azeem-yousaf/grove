package ui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/plan"
)

func TestGroupsHaveTheirOwnPlanner(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	a, c := m.sessionFor(m.taskPath(p, "feature/a")), m.sessionFor(m.taskPath(p, "feature/c"))
	a.lastReply.WriteString("Should the API return JSON or XML?")
	c.lastReply.WriteString("Which colour?")
	a.running, c.running = true, true
	m.onAgentDone(a, agent.Event{Kind: agent.EvDone})
	pump(t, m, m.onAgentDone(c, agent.Event{Kind: agent.EvDone}))

	crew := m.crews[crewKey(p.ID, "g")]
	if crew == nil || m.planner == nil {
		t.Fatalf("want the group's planner and the lead, got crew %v lead %v", crew != nil, m.planner != nil)
	}
	var toCrew, toLead *agent.Request
	for i, r := range fake.reqs {
		switch {
		case r.ReadOnly && r.Slot != "":
			toCrew = &fake.reqs[i]
		case r.ReadOnly:
			toLead = &fake.reqs[i]
		}
	}
	if toCrew == nil || !strings.Contains(toCrew.Message, "JSON or XML?") || strings.Contains(toCrew.Message, "Which colour?") ||
		!strings.Contains(toCrew.Context, `the group "Checkout" of the plan "Shop"`) || !strings.Contains(toCrew.Context, "feature/b") || strings.Contains(toCrew.Context, "feature/c") {
		t.Errorf("group planner's request: %+v", toCrew)
	}
	if toLead == nil || !strings.Contains(toLead.Message, "Which colour?") || strings.Contains(toLead.Message, "JSON or XML?") {
		t.Errorf("lead's request: %+v", toLead)
	}
	if crew.name != "planner · Checkout" || crew.turnAttr.Group != "g" || crew.turnAttr.Plan != p.ID {
		t.Errorf("crew %q attr %+v", crew.name, crew.turnAttr)
	}

	// Its decisions stay within its group.
	d, outs := m.scopeDecisions(crew, decisions{
		Reply: []replyDecision{{Branch: "feature/b", Message: "JSON"}, {Branch: "feature/c", Message: "blue"}},
		Post:  []postDecision{{Group: "other", Message: "hi"}},
		Merge: []planDecision{{Plan: p.ID}},
	})
	if len(d.Reply) != 1 || d.Reply[0].Branch != "feature/b" || d.Post[0].Group != "g" || d.Post[0].Plan != p.ID || len(d.Merge) != 0 {
		t.Errorf("scoped = %+v", d)
	}
	if len(outs) != 2 || outs[0].result != control.NotFound || outs[1].result != control.Failed {
		t.Errorf("refused = %+v", outs)
	}

	// Saved and restored as the same group's planner.
	if f := crew.file(); !strings.HasPrefix(f, crewDir+"/") {
		t.Errorf("saved as %q", f)
	}
	if r := restore(crew.saved(), true, true); r.crewKey() != crew.crewKey() || !r.planner {
		t.Errorf("restored %+v", r)
	}
}

func TestOpenAGroupsPlanner(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	m.rebuildRows()
	gi := slices.IndexFunc(m.rows, func(r row) bool { return r.kind == rowGroup })
	if gi < 0 {
		t.Fatal("no group row")
	}
	m.cursor, m.focus = gi, focusList
	press(m, "P")
	s := m.chatSession()
	if m.focus != focusChat || s == nil || s.crewGroup != "g" {
		t.Fatalf("P on a group should open its planner: focus %v session %+v", m.focus, s)
	}
	if out := plain(m); !strings.Contains(out, "planner · Checkout") {
		t.Errorf("chat:\n%s", out)
	}
	// ctrl+g goes to its group chat, and ctrl+p back to its planner.
	_, open := m.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	drive(t, m).until(open, func() bool { return !m.pollingGroups })
	if m.focus != focusGroup || m.groupChat.group != "g" {
		t.Fatalf("ctrl+g: focus %v", m.focus)
	}
	if out := plain(m); !strings.Contains(out, "ctrl+p its planner") {
		t.Errorf("group chat hints:\n%s", out)
	}
	m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if s := m.chatSession(); m.focus != focusChat || s == nil || s.crewGroup != "g" {
		t.Fatal("ctrl+p in the group chat should open its planner")
	}

	// A plan the user watches has no group planners.
	p.Orchestrated = false
	m.focus = focusList
	press(m, "P")
	if s := m.chatSession(); s == nil || s.crewGroup != "" {
		t.Errorf("P should open the lead planner: %+v", s)
	}
	_ = plan.ChatID
}

func TestRoleModels(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	m.cfg.AI = config.AI{ClaudeModel: "opus"}
	m.cfg.Planner.Model, m.cfg.Planner.CrewModel, m.cfg.Planner.TaskModel = "claude-opus-4-1", "sonnet", "haiku"

	lead, crew := m.plannerFor(p.ID, ""), m.plannerFor(p.ID, "g")
	if lead == nil || crew == nil || lead.model != "claude-opus-4-1" || crew.model != "sonnet" {
		t.Fatalf("lead %+v crew %+v", lead, crew)
	}
	path := m.taskPath(p, "feature/a")
	delete(m.sessions, path)
	task := m.newSession(path, "feature/a", agent.Claude)
	if task.model != "haiku" {
		t.Errorf("task model = %q", task.model)
	}
	// A worktree outside the plan keeps [ai]'s.
	if s := m.newSession(m.plannerDir(), "main", agent.Claude); s.model != "opus" {
		t.Errorf("worktree agent model = %q", s.model)
	}
	// Each keeps a Claude process of its own, so their models never make
	// another's restart.
	keys := map[string]bool{}
	for _, s := range []*session{lead, crew, task, m.sessionFor(m.plannerDir())} {
		r := m.agentRequest(s)
		keys[r.Dir+"\x00"+fmt.Sprint(r.ReadOnly)+"\x00"+r.Slot] = true
	}
	if len(keys) != 4 {
		t.Errorf("kept processes = %d, want 4", len(keys))
	}

	// Claude names aren't given to Codex: it falls back to [ai].
	m.cfg.AI.CodexModel = "gpt-5"
	m.crews = map[string]*session{}
	p.Agent = "codex"
	if s := m.plannerFor(p.ID, "g"); s == nil || s.model != "gpt-5" {
		t.Errorf("codex crew = %+v", s)
	}
	delete(m.sessions, path)
	if s := m.newSession(path, "feature/a", agent.Codex); s.model != "gpt-5" {
		t.Errorf("codex task model = %q", s.model)
	}

	// /model still changes it for that chat.
	pump(t, m, m.setModel(crew, "haiku"))
	if r := m.agentRequest(crew); r.Model != "haiku" {
		t.Errorf("after /model: %q", r.Model)
	}
}
