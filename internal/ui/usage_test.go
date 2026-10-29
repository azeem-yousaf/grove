package ui

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/usage"
	"github.com/charmbracelet/x/ansi"
)

// withLedger gives m a usage ledger in a temporary folder.
func withLedger(t *testing.T, m *Model) string {
	t.Helper()
	dir := t.TempDir()
	l, err := usage.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.usage = l
	return dir
}

// done ends a turn with figures.
func done(input, output int64, status usage.Status) agent.Event {
	return agent.Event{Kind: agent.EvDone, Tokens: usage.Tokens{Input: input, Output: output, HasCache: true}, TokenStatus: status}
}

// turnWith streams usage n times before ending with fin.
func turnWith(n int, fin agent.Event) []agent.Event {
	var evs []agent.Event
	for i := range n {
		evs = append(evs, agent.Event{Kind: agent.EvUsage, Tokens: usage.Tokens{Input: int64(i), Output: int64(i)}})
	}
	return append(evs, fin)
}

// agentSession makes the worktree agent for path, talking to fake.
func agentSession(m *Model, path, name string, fake *fakeAgent) *session {
	m.runAgent = fake.run
	s := m.newSession(filepath.FromSlash(path), name, agent.Claude)
	s.checkpoint = "taken" // no checkpoint from a folder that doesn't exist
	return s
}

func TestUsageAddsUpAcrossTurnsAndRestarts(t *testing.T) {
	m := fakeModel(t, 140, 36)
	dir := withLedger(t, m)
	fake := &fakeAgent{turns: [][]agent.Event{
		turnWith(50, done(100, 20, usage.Exact)),
		turnWith(50, done(200, 30, usage.Exact)),
		turnWith(50, done(300, 40, usage.Exact)),
	}}
	s := agentSession(m, "/src/shop-auth", "feature/auth", fake)
	for range 3 {
		pump(t, m, m.send(s, "go on"))
	}
	sum := m.usage.Agent(s.uid)
	if sum.Turns != 3 || sum.Tokens.Total() != 690 || sum.Exact != 3 {
		t.Fatalf("after three turns: %+v", sum)
	}
	// Usage streamed during the turns is shown, never recorded.
	if n := m.usage.Total().Turns; n != 3 {
		t.Errorf("ledger has %d turns, want 3", n)
	}
	if s.running || !s.live.IsZero() || s.turnID != "" {
		t.Errorf("live figures left after the turn: %+v %q", s.live, s.turnID)
	}
	if got := ansi.Strip(m.agentUsage(s)); got != "690 tok" {
		t.Errorf("chat header usage = %q", got)
	}

	// Closed in the middle of a fourth turn, with usage reported so far.
	m.startTurn(s)
	s.running = true
	s.live, s.liveSeen = usage.Tokens{Input: 40, Output: 2}, true
	b, err := json.Marshal(s.saved())
	if err != nil {
		t.Fatal(err)
	}
	load := func() (*Model, *session) {
		var v savedSession
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		m2 := fakeModel(t, 140, 36)
		l, err := usage.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		m2.usage = l
		r := restore(v, true, false)
		m2.onSessionsLoaded(sessionsLoadedMsg{sessions: []*session{r}})
		return m2, r
	}
	m2, r := load()
	if r.uid != s.uid || r.turns != 4 {
		t.Fatalf("restored uid %q turns %d, want %q 4", r.uid, r.turns, s.uid)
	}
	sum = m2.usage.Agent(r.uid)
	if sum.Turns != 4 || sum.Partial != 1 || sum.Tokens.Total() != 732 || sum.Complete() {
		t.Errorf("after a restart mid-turn: %+v", sum)
	}
	if got := ansi.Strip(m2.agentUsage(r)); got != "≥732 tok" {
		t.Errorf("cut short usage = %q", got)
	}
	// Loading the same saved file again records nothing new.
	m3, _ := load()
	if err := m3.usage.Reload(); err != nil {
		t.Fatal(err)
	}
	if sum := m3.usage.Agent(r.uid); sum.Turns != 4 || sum.Partial != 1 {
		t.Errorf("loaded twice: %+v", sum)
	}

	// The next turn after the restart counts on.
	fake2 := &fakeAgent{turns: [][]agent.Event{{done(10, 0, usage.Exact)}}}
	m2.runAgent = fake2.run
	r.checkpoint = "taken"
	pump(t, m2, m2.send(r, "carry on"))
	if sum := m2.usage.Agent(r.uid); sum.Turns != 5 || sum.Tokens.Total() != 742 {
		t.Errorf("after a turn following the restart: %+v", sum)
	}
}

func TestUsageSessionTotalComesBack(t *testing.T) {
	m := fakeModel(t, 140, 36)
	first := done(10, 1, usage.Exact)
	first.Session, first.SessionTotal = "s1", usage.Tokens{Input: 500, Output: 50}
	fake := &fakeAgent{turns: [][]agent.Event{{first}, {done(1, 1, usage.Exact)}}}
	s := agentSession(m, "/src/shop-auth", "feature/auth", fake)
	pump(t, m, m.send(s, "one"))

	// Saved with the session, and given back on the next turn after a restart.
	r := restore(s.saved(), true, false)
	if r.sessionTotal != first.SessionTotal {
		t.Fatalf("restored session total = %+v", r.sessionTotal)
	}
	r.checkpoint = "taken"
	pump(t, m, m.send(r, "two"))
	if got := fake.reqs[1].SessionTotal; got != first.SessionTotal {
		t.Errorf("next request's session total = %+v", got)
	}
	// A new Claude session starts from nothing.
	r.setSessionID("s2")
	if !r.sessionTotal.IsZero() || !m.agentRequest(r).SessionTotal.IsZero() {
		t.Errorf("session total kept for a new session: %+v", r.sessionTotal)
	}
}

func TestUsageCutShortWithoutFigures(t *testing.T) {
	m := fakeModel(t, 140, 36)
	withLedger(t, m)
	s := m.newSession(filepath.FromSlash("/src/shop-auth"), "feature/auth", agent.Codex)
	m.startTurn(s)
	s.running = true
	r := restore(s.saved(), true, false)
	m.sessions = map[string]*session{}
	m.onSessionsLoaded(sessionsLoadedMsg{sessions: []*session{r}})
	sum := m.usage.Agent(s.uid)
	if sum.Turns != 1 || sum.Unknown != 1 {
		t.Fatalf("cut short with nothing reported: %+v", sum)
	}
	if got := ansi.Strip(m.agentUsage(r)); got != "usage n/a" {
		t.Errorf("unknown usage shows %q, want usage n/a", got)
	}
}

func TestUsageOldSessionGetsUID(t *testing.T) {
	r := restore(savedSession{Version: 1, Kind: agent.Claude, Path: "/x"}, true, false)
	if r.uid == "" {
		t.Error("a session saved before usage tracking has no uid")
	}
}

func TestUsageStoppedAndFailedTurns(t *testing.T) {
	m := fakeModel(t, 140, 36)
	withLedger(t, m)
	stopped := done(50, 5, usage.Exact)
	stopped.Err = errors.New("stopped")
	failed := agent.Event{Kind: agent.EvDone, Err: errors.New("boom"), TokenStatus: usage.Unknown}
	fake := &fakeAgent{turns: [][]agent.Event{{stopped}, {failed}}}
	s := agentSession(m, "/src/shop-auth", "feature/auth", fake)
	pump(t, m, m.send(s, "one"))
	pump(t, m, m.send(s, "two"))
	sum := m.usage.Agent(s.uid)
	if sum.Turns != 2 || sum.Failed != 2 || sum.Unknown != 1 || sum.Tokens.Total() != 55 {
		t.Errorf("stopped and failed turns: %+v", sum)
	}
	// Removing the worktree keeps its usage.
	m.forgetSession(s.path)
	if sum := m.usage.Agent(s.uid); sum.Turns != 2 {
		t.Errorf("after removal: %+v", sum)
	}
}

func TestUsagePlannerDraftingTurnsGoToThePlan(t *testing.T) {
	m := fakeModel(t, 140, 36)
	withLedger(t, m)
	reply := "Here it is.\n\n```grove-plan\n{\"title\": \"Tokens\", \"tasks\": [{\"id\": \"a\", \"title\": \"A\", \"branch\": \"feature/x\", \"brief\": \"b\"}]}\n```"
	fake := &fakeAgent{turns: [][]agent.Event{
		{{Kind: agent.EvText, Text: "What should it count?"}, done(10, 1, usage.Exact)},
		{{Kind: agent.EvText, Text: reply}, done(20, 2, usage.Exact)},
		{{Kind: agent.EvText, Text: "Sure."}, done(30, 3, usage.Exact)},
	}}
	m.runAgent = fake.run
	m.planner = m.newPlanner(filepath.FromSlash("/src/shop"), agent.Claude)
	pump(t, m, m.send(m.planner, "count tokens"))
	pump(t, m, m.send(m.planner, "everything"))
	d := m.draft()
	if d == nil {
		t.Fatal("no draft plan")
	}
	if sum := m.usage.Plan(d.ID); sum.Turns != 2 || sum.Tokens.Total() != 33 {
		t.Errorf("draft plan's usage = %+v, want both drafting turns", sum)
	}
	// A turn after the draft waits for the next one.
	pump(t, m, m.send(m.planner, "thanks"))
	if sum := m.usage.PlannerIn(""); sum.Turns != 1 {
		t.Errorf("planner turns without a plan = %+v", sum)
	}
	if len(m.planner.drafting) != 1 {
		t.Errorf("drafting = %v", m.planner.drafting)
	}
}

// usagePlan gives m a running plan watched by the planner: a group with the
// auth and cart worktrees, and an isolated task not started yet.
func usagePlan(m *Model) *plan.Plan {
	p := &plan.Plan{ID: "p1", Title: "Token usage", Status: plan.Running, Orchestrated: true,
		Groups: []plan.Group{{ID: "g", Name: "Counting"}},
		Tasks: []plan.Task{
			{ID: "a", Title: "Auth", Branch: "feature/auth", Path: filepath.FromSlash("/src/shop-auth"), Group: "g", Status: plan.Working},
			{ID: "c", Title: "Cart", Branch: "feature/cart", Path: filepath.FromSlash("/src/shop-cart"), Group: "g", Status: plan.Working},
			// Working without a worktree, so nothing tries to create one.
			{ID: "i", Title: "Docs", Branch: "docs/usage", Status: plan.Working},
		}}
	m.plans = []*plan.Plan{p}
	m.rebuildRows()
	return p
}

func TestUsageGroupAndPlanTotals(t *testing.T) {
	m := fakeModel(t, 160, 40)
	withLedger(t, m)
	p := usagePlan(m)
	// Each agent's turns cost the same, however often grove wakes it (a
	// finished task wakes its group and the watching planner).
	per := map[string]int64{"shop-auth": 100, "shop-cart": 200, "shop": 300}
	m.runAgent = func(_ context.Context, req agent.Request, emit func(agent.Event)) {
		emit(done(per[filepath.Base(req.Dir)], 0, usage.Exact))
	}
	auth := m.newSession(filepath.FromSlash("/src/shop-auth"), "feature/auth", agent.Claude)
	cart := m.newSession(filepath.FromSlash("/src/shop-cart"), "feature/cart", agent.Claude)
	auth.checkpoint, cart.checkpoint = "taken", "taken"
	m.planner = m.newPlanner(filepath.FromSlash("/src/shop"), agent.Claude)
	pump(t, m, m.send(auth, "go"))
	pump(t, m, m.send(cart, "go"))
	pump(t, m, m.send(m.planner, "how is it going?"))

	a, c, pl := m.usage.Agent(auth.uid), m.usage.Agent(cart.uid), m.usage.Agent(m.planner.uid)
	if a.Turns == 0 || c.Turns == 0 || pl.Turns == 0 {
		t.Fatalf("turns: auth %+v cart %+v planner %+v", a, c, pl)
	}
	group := a.Tokens.Total() + c.Tokens.Total()
	if got := m.usage.Group(p.ID, "g").Tokens.Total(); got != group {
		t.Errorf("group total = %d, want auth + cart = %d", got, group)
	}
	if got := m.usage.Task(p.ID, "c"); got.Tokens.Total() != c.Tokens.Total() || got.Turns != c.Turns {
		t.Errorf("cart task = %+v, want %+v", got, c)
	}
	if got := m.usage.PlannerIn(p.ID); got.Tokens.Total() != pl.Tokens.Total() || m.usage.PlannerIn("").Turns != 0 {
		t.Errorf("planner's share = %+v, want all of %+v", got, pl)
	}
	total := group + pl.Tokens.Total()
	if got := m.usage.Plan(p.ID).Tokens.Total(); got != total {
		t.Errorf("plan total = %d, want %d", got, total)
	}

	// The group's summary and the overview show them.
	m.focus = focusList
	m.moveTo(m.groupRow(0))
	if lines := ansi.Strip(strings.Join(m.groupSummaryLines(0, 80), "\n")); !strings.Contains(lines, usage.Compact(group)+" tok") || !strings.Contains(lines, "T for details") {
		t.Errorf("group summary lacks %s tok:\n%s", usage.Compact(group), lines)
	}
	press(m, "w")
	if out := plain(m); !strings.Contains(out, usage.Compact(total)+" tok") {
		t.Errorf("overview lacks the plan's %s tok:\n%s", usage.Compact(total), out)
	}

	// T from the overview opens the usage screen; esc goes back.
	press(m, "T")
	out := plain(m)
	for _, want := range []string{"Token usage", "All agents", "Token usage", "Counting", "feature/cart", "docs/usage", "planner", "Top agents", "TOTAL"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage screen lacks %q:\n%s", want, out)
		}
	}
	press(m, "esc")
	if m.usageView || !m.overview {
		t.Error("esc should close the usage screen, back to the overview")
	}
}

func TestUsageScreenMarksLiveRemovedAndUnknown(t *testing.T) {
	m := fakeModel(t, 160, 40)
	withLedger(t, m)
	gone := "/src/shop-gone"
	m.usage.SetAgent(usage.AgentInfo{ID: "gone1", Kind: "codex", Path: filepath.FromSlash(gone), Branch: "feature/gone"})
	m.usage.Add(usage.Turn{ID: "gone1/1", Agent: "gone1", Status: usage.Unknown})

	s := m.newSession(filepath.FromSlash("/src/shop-auth"), "feature/auth", agent.Claude)
	m.startTurn(s)
	s.running = true
	s.live, s.liveSeen = usage.Tokens{Input: 2300}, true
	if got := ansi.Strip(m.agentUsage(s)); got != "+ 2.3k live" {
		t.Errorf("live usage = %q", got)
	}

	press(m, "T")
	out := plain(m)
	for _, want := range []string{"feature/gone · codex (removed)", "n/a", "+2.3k", "LIVE", "cut short", "not reported"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage screen lacks %q:\n%s", want, out)
		}
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "feature/gone") && strings.Contains(l, " 0 ") {
			t.Errorf("unknown usage shows as 0: %q", l)
		}
	}
	press(m, "esc")
	if m.usageView {
		t.Error("esc should close the usage screen")
	}
}

// fillUsage records turns of every kind for the render tests: exact ones
// with and without cache figures, one cut short, one not reported, an agent
// whose worktree is gone, and one turn still running.
func fillUsage(t *testing.T, m *Model, many bool) {
	t.Helper()
	withLedger(t, m)
	p := usagePlan(m)
	p.Title = strings.Repeat("A long plan title ", 6)
	l := m.usage
	agents := []usage.AgentInfo{
		{ID: "auth", Kind: "claude", Path: filepath.FromSlash("/src/shop-auth"), Branch: "feature/auth"},
		{ID: "cart", Kind: "codex", Path: filepath.FromSlash("/src/shop-cart"), Branch: "feature/cart"},
		{ID: "plnr", Kind: "claude", Path: filepath.FromSlash("/src/shop"), Planner: true},
		{ID: "gone", Kind: "codex", Path: filepath.FromSlash("/src/" + strings.Repeat("gone-", 20)), Branch: "feature/" + strings.Repeat("gone-", 20)},
	}
	for _, a := range agents {
		l.SetAgent(a)
	}
	add := func(id, ag string, tok usage.Tokens, st usage.Status, task, grp string) {
		l.Add(usage.Turn{ID: id, Agent: ag, Tokens: tok, Status: st, Plan: p.ID, PlanTitle: p.Title, Task: task, Group: grp})
	}
	add("auth/1", "auth", usage.Tokens{Input: 1_234_567, CacheRead: 1_000_000, Output: 45_678, HasCache: true}, usage.Exact, "a", "g")
	add("cart/1", "cart", usage.Tokens{Input: 9_876, Output: 543}, usage.Exact, "c", "g")
	add("plnr/1", "plnr", usage.Tokens{Input: 5_000, Output: 600, HasCache: true}, usage.Partial, "", "")
	l.Add(usage.Turn{ID: "plnr/2", Agent: "plnr", Status: usage.Unknown})
	l.Add(usage.Turn{ID: "gone/1", Agent: "gone", Tokens: usage.Tokens{Input: 99_999_999, Output: 1}, Status: usage.Exact})
	if many {
		for i := range 30 {
			id := "x" + strings.Repeat("y", i)
			l.SetAgent(usage.AgentInfo{ID: id, Kind: "claude", Branch: "feature/many-" + id})
			l.Add(usage.Turn{ID: id + "/1", Agent: id, Tokens: usage.Tokens{Input: int64(i)}, Status: usage.Exact})
		}
	}
	s := m.newSession(filepath.FromSlash("/src/shop-auth"), "feature/auth", agent.Claude)
	m.startTurn(s)
	s.running = true
	s.live, s.liveSeen = usage.Tokens{Input: 2_300}, true
}

func TestUsageWithoutLedger(t *testing.T) {
	m := fakeModel(t, 140, 36)
	fake := &fakeAgent{turns: [][]agent.Event{{done(1, 1, usage.Exact)}}}
	s := agentSession(m, "/src/shop-auth", "feature/auth", fake)
	pump(t, m, m.send(s, "go"))
	if m.agentUsage(s) != "" {
		t.Error("usage shown without a ledger")
	}
	press(m, "T")
	if out := plain(m); !strings.Contains(out, "isn't being recorded") {
		t.Errorf("usage screen without a ledger:\n%s", out)
	}
}
