package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
)

// copied returns the toast texts of the copy actions cmd runs.
func copied(cmd tea.Cmd) []string {
	var out []string
	for _, msg := range runCmds(cmd) {
		if a, ok := msg.(actionMsg); ok {
			out = append(out, a.text)
		}
	}
	return out
}

func TestChatPasteKeepsLines(t *testing.T) {
	m := fakeModel(t, 120, 36)
	fake := &fakeAgent{}
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	m.runAgent = fake.run
	press(m, "a")

	m.Update(tea.PasteMsg{Content: "Fix these:\n1. login\n2. logout"})
	typeText(m, " now")
	if got := m.chatInput.Value(); got != "Fix these:\n1. login\n2. logout now" {
		t.Fatalf("input = %q", got)
	}
	if !strings.Contains(plain(m), "2. logout now") {
		t.Errorf("every pasted line should show in the input:\n%s", plain(m))
	}
	// shift+enter adds a line; enter sends.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	typeText(m, "thanks")
	pump(t, m, func() tea.Msg { _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); return cmd() })
	if len(fake.reqs) != 1 || fake.reqs[0].Message != "Fix these:\n1. login\n2. logout now\nthanks" {
		t.Fatalf("sent %+v", fake.reqs)
	}
	if m.chatInput.Value() != "" {
		t.Error("sending should clear the input")
	}
}

func TestChatCopy(t *testing.T) {
	m := fakeModel(t, 120, 36)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	press(m, "a")
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if m.toast.text != "The agent hasn't replied yet" || cmd == nil {
		t.Errorf("ctrl+y with no reply: toast %q", m.toast.text)
	}

	s := m.chatSession()
	s.entries = []chatEntry{
		{roleUser, "make it faster"},
		{roleAgent, "Done. **Cached** the lookups:\n\n```go\nvar cache = map[string]int{}\n```"},
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if got := copied(cmd); len(got) != 1 || got[0] != "Copied the agent's reply" {
		t.Errorf("ctrl+y copied %v", got)
	}

	// Dragging over the conversation copies those lines as plain text.
	plain(m)
	x, top, _, _ := m.chatArea()
	lines := m.chatLines(s, m.chatView.Width())
	first, last := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "Done.") {
			first = i
		}
		if strings.Contains(l, "var cache") {
			last = i
		}
	}
	if first < 0 || last < 0 {
		t.Fatalf("reply not rendered: %q", lines)
	}
	m.Update(tea.MouseClickMsg{X: x + 3, Y: top + first, Button: tea.MouseLeft})
	m.Update(tea.MouseMotionMsg{X: x + 10, Y: top + last, Button: tea.MouseLeft})
	if !m.chatSel.has(first) || !m.chatSel.has(last) {
		t.Fatalf("selection = %+v, want lines %d-%d", m.chatSel, first, last)
	}
	_, cmd = m.Update(tea.MouseReleaseMsg{X: x + 10, Y: top + last, Button: tea.MouseLeft})
	if got := copied(cmd); len(got) != 1 || got[0] != "Copied "+plural(last-first+1, "line") {
		t.Errorf("drag copied %v", got)
	}
	got := selectedChatText(lines[first : last+1])
	if got != "Done. Cached the lookups:\n\n  var cache = map[string]int{}" {
		t.Errorf("selected text = %q", got)
	}

	// A plain click selects nothing, and typing clears a selection.
	m.Update(tea.MouseClickMsg{X: x + 3, Y: top + first, Button: tea.MouseLeft})
	_, cmd = m.Update(tea.MouseReleaseMsg{X: x + 3, Y: top + first, Button: tea.MouseLeft})
	if m.chatSel.active || cmd != nil {
		t.Error("a click should not select or copy")
	}
}

func TestChatShowsTestResults(t *testing.T) {
	m := fakeModel(t, 120, 36)
	fake := &fakeAgent{turns: [][]agent.Event{{
		{Kind: agent.EvTool, Tool: "Bash", Detail: "go test ./..."},
		{Kind: agent.EvOutput, Tool: "Bash", Detail: "go test ./...", Failed: true, Text: "--- FAIL: TestLogin\nFAIL\tshop/auth\t0.1s"},
		{Kind: agent.EvTool, Tool: "Bash", Detail: "ls"},
		{Kind: agent.EvOutput, Tool: "Bash", Detail: "ls", Text: "a.go"},
		{Kind: agent.EvText, Text: "The login test fails."},
	}}}
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	m.runAgent = fake.run
	press(m, "a")
	typeText(m, "run the tests")
	pump(t, m, func() tea.Msg { _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); return cmd() })
	out := plain(m)
	for _, want := range []string{"✗ failed: go test ./...", "--- FAIL: TestLogin", "shop/auth"} {
		if !strings.Contains(out, want) {
			t.Errorf("chat should show %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "a.go") {
		t.Error("only test commands' output shows")
	}
	if !isTestCommand("npm run test:unit") || !isTestCommand("pytest -q") || isTestCommand("git log --grep test") || isTestCommand("cat latest.txt") {
		t.Error("isTestCommand")
	}
}
