package ui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/store"
	"github.com/azeem-yousaf/grove/internal/usage"
)

// enterChat types text into the chat and presses enter, running what that
// starts.
func enterChat(t *testing.T, m *Model, text string) {
	t.Helper()
	typeText(m, text)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)
}

// /clear starts the agent on a new conversation: nothing of the old one goes
// with the next turn, which is briefed again, but undo and usage stay.
func TestClearChat(t *testing.T) {
	m := fakeModel(t, 140, 36)
	withLedger(t, m)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	var stopped []string
	m.stopAgent = func(key string) { stopped = append(stopped, key) }
	fa := &fakeAgent{turns: [][]agent.Event{
		{{Kind: agent.EvSession, Session: "s1"}, {Kind: agent.EvText, Text: "Old reply."}, done(100, 10, usage.Exact)},
		{{Kind: agent.EvSession, Session: "s2"}, {Kind: agent.EvText, Text: "New reply."}, done(50, 5, usage.Exact)},
	}}
	m.runAgent = fa.run
	auth := filepath.FromSlash("/src/shop-auth")

	press(m, "a")
	old := m.chatSession()
	old.checkpoint = "tree" // skip git in this test
	enterChat(t, m, "add a login page")
	if old.id != "s1" || !strings.Contains(plain(m), "Old reply.") {
		t.Fatalf("first turn: id=%q", old.id)
	}
	old.allow = []string{agent.ClaudeRule("Bash", "npm test")}
	old.notes = []string{"a note"}
	m.store = store.Open(t.TempDir())
	m.persistNow()
	if b, _ := os.ReadFile(filepath.Join(m.store.Dir(), filepath.FromSlash(sessionFile(auth)))); !strings.Contains(string(b), "Old reply.") {
		t.Fatalf("session not saved: %s", b)
	}

	enterChat(t, m, "/clear")
	m.persistNow()
	if b, _ := os.ReadFile(filepath.Join(m.store.Dir(), filepath.FromSlash(sessionFile(auth)))); strings.Contains(string(b), "Old reply.") || !strings.Contains(string(b), "Chat cleared.") {
		t.Errorf("saved session after /clear: %s", b)
	}
	m.store = nil
	s := m.chatSession()
	if s == old || m.sessionFor(auth) != s {
		t.Fatal("/clear should make a new session")
	}
	if s.id != "" || s.uid == old.uid || len(s.allow) != 0 || len(s.notes) != 0 || s.checkpoint != "tree" {
		t.Errorf("new session: id=%q uid same=%v allow=%v notes=%v checkpoint=%q", s.id, s.uid == old.uid, s.allow, s.notes, s.checkpoint)
	}
	if len(s.entries) != 1 || s.entries[0].text != "Chat cleared. The agent starts a new conversation." {
		t.Errorf("entries = %+v", s.entries)
	}
	if out := plain(m); strings.Contains(out, "Old reply.") || !strings.Contains(out, "Chat cleared.") {
		t.Errorf("chat after /clear:\n%s", out)
	}
	if !slices.Contains(stopped, auth) {
		t.Errorf("the kept process should be stopped: %v", stopped)
	}
	if m.chatInput.Value() != "" || len(fa.reqs) != 1 {
		t.Errorf("/clear must not be sent: input %q, requests %d", m.chatInput.Value(), len(fa.reqs))
	}

	enterChat(t, m, "start over")
	req := fa.reqs[1]
	if req.Session != "" || len(req.History) != 0 || !strings.Contains(req.Context, "never read or change anything in them") {
		t.Errorf("turn after /clear: session=%q history=%d context=%q", req.Session, len(req.History), req.Context)
	}
	for _, r := range req.Allow {
		if r == agent.ClaudeRule("Bash", "npm test") {
			t.Error("approvals must not carry over")
		}
	}

	// Undo still reaches back to before the first turn.
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl})
	if cmd == nil || strings.Contains(m.toast.text, "Nothing to undo") {
		t.Error("ctrl+z should still offer undo")
	}

	writeUsage(t, m)
	if sum := m.usage.Agent(old.uid); sum.Turns != 1 || sum.Tokens.Total() != 110 {
		t.Errorf("old conversation's usage = %+v", sum)
	}
	if sum := m.usage.Agent(s.uid); sum.Turns != 1 || sum.Tokens.Total() != 55 {
		t.Errorf("new conversation's usage = %+v", sum)
	}
}

func TestClearChatCodexForgetsHistory(t *testing.T) {
	m := fakeModel(t, 140, 36)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Codex} }
	fa := &fakeAgent{turns: [][]agent.Event{{{Kind: agent.EvText, Text: "one"}}, {{Kind: agent.EvText, Text: "two"}}}}
	m.runAgent = fa.run
	press(m, "a")
	m.chatSession().checkpoint = "tree"
	enterChat(t, m, "first")
	enterChat(t, m, "/clear")
	enterChat(t, m, "second")
	if len(fa.reqs) != 2 || len(fa.reqs[1].History) != 0 || fa.reqs[1].Context == "" {
		t.Errorf("Codex after /clear: %+v", fa.reqs)
	}
}

func TestClearChatRefused(t *testing.T) {
	m := fakeModel(t, 140, 36)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	press(m, "a")
	s := m.chatSession()
	s.running = true
	enterChat(t, m, "/clear")
	if m.chatSession() != s || len(s.queued) != 0 || !strings.Contains(m.toast.text, "Stop the agent first") {
		t.Errorf("while running: queued %v, toast %q", s.queued, m.toast.text)
	}
	if m.chatInput.Value() != "" {
		t.Error("the input should be cleared")
	}
	s.running = false

	// '/clear something' is an ordinary message.
	fa := &fakeAgent{}
	m.runAgent = fa.run
	s.checkpoint = "tree"
	enterChat(t, m, "/clear the cache")
	if len(fa.reqs) != 1 || fa.reqs[0].Message != "/clear the cache" {
		t.Errorf("/clear with words: %+v", fa.reqs)
	}
}

func TestClearPlannerChat(t *testing.T) {
	m := fakeModel(t, 140, 36)
	var stopped []string
	m.stopAgent = func(key string) { stopped = append(stopped, key) }
	shop := filepath.FromSlash("/src/shop")
	old := m.newPlanner(shop, agent.Claude)
	old.id, old.model = "s1", "haiku"
	old.entries = []chatEntry{{roleAgent, "Here's a plan."}}
	m.planner = old
	pump(t, m, m.showChat(old))

	// Not while it watches a plan.
	m.plans = []*plan.Plan{{ID: "p1", Title: "Shop", Status: plan.Running, Orchestrated: true}}
	enterChat(t, m, "/clear")
	if m.planner != old || !strings.Contains(m.toast.text, "forget the plan") {
		t.Errorf("watching planner cleared, toast %q", m.toast.text)
	}

	// A plan it doesn't watch doesn't stop it.
	m.plans[0].Orchestrated = false
	enterChat(t, m, "/clear")
	s := m.planner
	if s == old || !s.planner || s.id != "" || s.uid == old.uid || s.model != "haiku" || m.chatSession() != s {
		t.Fatalf("planner after /clear: %+v", s)
	}
	if !slices.Contains(stopped, agent.ReadOnlyKey(shop, "")) {
		t.Errorf("the planner's process should be stopped: %v", stopped)
	}
	if strings.Contains(plain(m), "Here's a plan.") {
		t.Error("the old chat should be gone")
	}

	// A group's planner never.
	p := &plan.Plan{ID: "p2", Title: "Cart", Status: plan.Running, Orchestrated: true, Agent: "claude", Groups: []plan.Group{{ID: "g", Name: "Checkout"}}}
	m.plans = append(m.plans, p)
	crew := m.plannerFor("p2", "g")
	pump(t, m, m.showChat(crew))
	enterChat(t, m, "/clear")
	if m.crews[crew.crewKey()] != crew || !strings.Contains(m.toast.text, "forget the plan") {
		t.Errorf("group planner cleared, toast %q", m.toast.text)
	}
}

// The palette offers it for the chat on screen.
func TestClearChatInPalette(t *testing.T) {
	m := fakeModel(t, 140, 36)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	press(m, "ctrl+k")
	if slices.Contains(shownNames(m), "Clear chat") {
		t.Error("no chat on screen, nothing to clear")
	}
	press(m, "esc", "a")
	old := m.chatSession()
	press(m, "ctrl+k")
	if m.actions == nil || !slices.Contains(shownNames(m), "Clear chat") {
		t.Fatalf("palette in the chat: %v", m.actions)
	}
	typeText(m, "clear chat")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)
	if m.chatSession() == old || m.focus != focusChat {
		t.Error("Clear chat should clear it")
	}
}

// Group chats have no /clear: it is posted like any message.
func TestClearInGroupChatIsText(t *testing.T) {
	m := groupedFake(t, 140, 36)
	m.store = store.Open(t.TempDir())
	p := m.plans[0]
	m.openGroupChat(filepath.FromSlash("/src/shop-auth"))
	m.pollingGroups = false
	m.Update(m.pollGroups()())
	if m.focus != focusGroup {
		t.Fatal("group chat not open")
	}
	typeText(m, "/clear")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	runCmds(cmd)
	log, _ := m.groupLog(p, "g")
	msgs, _ := log.All()
	if len(msgs) == 0 || msgs[len(msgs)-1].Text != "/clear" {
		t.Errorf("group chat messages = %+v", msgs)
	}
}
