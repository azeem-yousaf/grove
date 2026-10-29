package ui

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/plan"
)

const plannerReply = "I'd split it in two.\n\n```grove-plan\n" + `{
  "title": "Guest checkout",
  "groups": [{"id": "checkout", "name": "Checkout flow"}],
  "tasks": [
    {"id": "api", "title": "Guest API", "branch": "feature/guest-api", "group": "checkout", "brief": "Add POST /checkout/guest."},
    {"id": "ui", "title": "Guest page", "branch": "feature/guest-ui", "group": "checkout", "brief": "Add the guest form."}
  ]
}` + "\n```"

// driver runs commands and feeds every resulting message back into the
// model, except timers. Results keep arriving across calls to until.
type driver struct {
	t       *testing.T
	m       *Model
	results chan tea.Msg
	running sync.WaitGroup
}

// drive returns a driver for m. Before the test's temporary folders are
// deleted it waits a while for commands still running, such as a save
// started by the last message: Windows can't delete a folder being written.
func drive(t *testing.T, m *Model) *driver {
	d := &driver{t: t, m: m, results: make(chan tea.Msg, 256)}
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { d.running.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second): // an agent listener may never return
		}
	})
	return d
}

func (d *driver) start(c tea.Cmd) {
	if c != nil {
		d.running.Add(1)
		go func() {
			defer d.running.Done()
			d.results <- c()
		}()
	}
}

// until starts cmd and processes messages until done reports true.
func (d *driver) until(cmd tea.Cmd, done func() bool) {
	d.t.Helper()
	d.start(cmd)
	deadline := time.After(30 * time.Second)
	for !done() {
		select {
		case msg := <-d.results:
			switch msg := msg.(type) {
			case tea.BatchMsg:
				for _, c := range msg {
					d.start(c)
				}
			case nil, clockMsg, frameMsg, toastClearMsg, diffDebounceMsg:
			default:
				_, next := d.m.Update(msg)
				d.start(next)
			}
		case <-deadline:
			d.t.Fatal("driver: still waiting after 30s")
		}
	}
}

// settle drives cmd until done.
func settle(t *testing.T, m *Model, cmd tea.Cmd, done func() bool) {
	t.Helper()
	drive(t, m).until(cmd, done)
}

func TestPlannerEndToEnd(t *testing.T) {
	m, mainDir := mergeModel(t)
	m.cfg.Planner.MaxAgents = 1
	fake := &fakeAgent{turns: [][]agent.Event{
		{{Kind: agent.EvText, Text: plannerReply}},
		{{Kind: agent.EvText, Text: "Done: added the endpoint."}},
		{{Kind: agent.EvText, Text: "Done: added the form."}},
	}}
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	m.runAgent = fake.run

	press(m, "P")
	if m.focus != focusChat || !m.chatPlanner || m.planner == nil || !git.SamePath(m.planner.path, mainDir) {
		t.Fatalf("P should open the planner in the main worktree: focus %d, planner %+v", m.focus, m.planner)
	}
	if !strings.Contains(plain(m), "Plan work across worktrees") {
		t.Errorf("the planner should explain itself:\n%s", plain(m))
	}
	typeText(m, "Add guest checkout")
	pump(t, m, func() tea.Msg { _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); return cmd() })
	req := fake.reqs[0]
	if !req.ReadOnly || !strings.Contains(req.Context, "You are grove's planner") || !strings.Contains(req.Context, "grove-plan") {
		t.Fatalf("planner request = %+v", req)
	}
	if m.sessionFor(mainDir) != nil {
		t.Error("the planner must not take the main worktree's agent slot")
	}

	// The proposal opens for review, and nothing exists yet.
	r := m.review
	if r == nil || len(r.p.Tasks) != 2 || r.p.Request != "Add guest checkout" {
		t.Fatalf("review = %+v", r)
	}
	if out := plain(m); !strings.Contains(out, "Review plan") || !strings.Contains(out, "⇄ Checkout flow") {
		t.Errorf("review screen:\n%s", out)
	}

	// Edit the second task onto a branch that exists: starting is refused.
	press(m, "down", "enter")
	r.edit.branch.SetValue("feature/a")
	press(m, "enter")
	_, cmd := m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	settle(t, m, cmd, func() bool { return !r.checking })
	if len(r.problems) != 1 || !strings.Contains(r.problems[0], "the branch feature/a already exists") {
		t.Fatalf("problems = %v", r.problems)
	}
	press(m, "enter")
	r.edit.branch.SetValue("feature/guest-ui")
	press(m, "enter")

	_, cmd = m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	var p *plan.Plan
	d := drive(t, m)
	d.until(cmd, func() bool { return m.review == nil })
	p = m.plans[0]
	if p.Status != plan.Running || p.Agent != "claude" {
		t.Fatalf("plan = %+v", p)
	}
	// One agent slot: the second task waits for the first.
	if p.Tasks[0].Status != plan.Working || p.Tasks[1].Status != plan.Queued {
		t.Fatalf("statuses = %s, %s; want working, queued", p.Tasks[0].Status, p.Tasks[1].Status)
	}

	d.until(nil, func() bool { return p.Status == plan.Done })
	for _, task := range p.Tasks {
		if task.Status != plan.Finished || task.Path == "" {
			t.Errorf("task %s = %+v", task.ID, task)
		}
		if _, err := os.Stat(task.Path); err != nil {
			t.Errorf("%s's worktree: %v", task.ID, err)
		}
		if s := m.sessionFor(task.Path); s == nil || s.kind != agent.Claude {
			t.Errorf("%s has no agent", task.ID)
		}
	}
	if len(fake.reqs) != 3 {
		t.Fatalf("got %d agent turns, want planner + 2 tasks", len(fake.reqs))
	}
	for i, want := range []string{"Add POST /checkout/guest.", "Add the guest form."} {
		ctx := fake.reqs[i+1].Context
		if fake.reqs[i+1].ReadOnly || !strings.Contains(ctx, "## Your task") || !strings.Contains(ctx, want) || !strings.Contains(ctx, `the plan "Guest checkout"`) {
			t.Errorf("task %d briefing:\n%s", i, ctx)
		}
	}
	last := m.planner.entries[len(m.planner.entries)-1].text
	if !strings.Contains(last, "Guest checkout is done: 2 tasks finished") {
		t.Errorf("planner's last note = %q", last)
	}

	// The plan is saved, and comes back after a restart.
	m.persistNow()
	m2 := reopen(t, m)
	if cmd := m2.loadPlans(); cmd != nil {
		m2.Update(cmd())
	}
	if len(m2.plans) != 1 || m2.plans[0].Status != plan.Done || m2.planner != nil && !m2.planner.planner {
		t.Errorf("after restart: plans %+v", m2.plans)
	}
	if m2.planner == nil || len(m2.planner.entries) != len(m.planner.entries) {
		t.Error("the planner conversation wasn't restored")
	}
}

func TestPlanReviewEditing(t *testing.T) {
	m := fakeModel(t, 120, 36)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	m.planner = m.newPlanner("/src/shop", agent.Claude)
	m.planner.entries = []chatEntry{{roleUser, "do it"}}

	// Proposed while the user is elsewhere: a toast, not a dialog.
	m.onPlannerReply(m.planner, plannerReply)
	if m.review != nil || !strings.Contains(m.toast.text, "press P to review") {
		t.Fatalf("review %v, toast %q", m.review, m.toast.text)
	}
	press(m, "P")
	press(m, "esc") // leave the chat; ctrl+o is the planner's key for the review
	m.showChat(m.planner)
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	r := m.review
	if r == nil {
		t.Fatal("ctrl+o should open the draft")
	}

	// g cycles a task through the groups: a new group, isolated, back.
	press(m, "down", "g")
	if ui := r.p.Tasks[1]; ui.Group == "checkout" || ui.Group == "" || len(r.p.Groups) != 2 {
		t.Fatalf("after g: group %q, groups %+v", ui.Group, r.p.Groups)
	}
	press(m, "g")
	if r.p.Tasks[1].Group != "" || len(r.p.Groups) != 1 {
		t.Fatalf("after g g: group %q, groups %+v", r.p.Tasks[1].Group, r.p.Groups)
	}
	press(m, "g")
	if r.p.Tasks[1].Group != "checkout" {
		t.Fatalf("after g g g: group %q", r.p.Tasks[1].Group)
	}
	press(m, "r")
	r.naming.SetValue("Checkout")
	press(m, "enter")
	if g, _ := r.p.Group("checkout"); g.Name != "Checkout" {
		t.Errorf("rename: %+v", g)
	}

	// Edits stay in the draft when the review closes, without touching
	// what was there before until then.
	press(m, "d")
	if m.draft().Tasks[1].ID != "ui" {
		t.Error("the draft changed before the review closed")
	}
	press(m, "esc")
	if d := m.draft(); m.review != nil || len(d.Tasks) != 1 || d.Groups[0].Name != "Checkout" {
		t.Errorf("draft after closing: %+v", d)
	}

	// A new proposal replaces the old draft.
	m.onPlannerReply(m.planner, plannerReply)
	if len(m.plans) != 1 || len(m.draft().Tasks) != 2 {
		t.Errorf("plans = %d, draft tasks %d", len(m.plans), len(m.draft().Tasks))
	}
	// A broken plan is reported, not dropped silently.
	m.onPlannerReply(m.planner, "```grove-plan\n{oops\n```")
	if e := m.planner.entries[len(m.planner.entries)-1]; e.role != roleError {
		t.Errorf("last entry = %+v", e)
	}
}
