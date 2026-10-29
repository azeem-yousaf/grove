package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/charmbracelet/x/ansi"
)

// leftColumn is the list side of each rendered line.
func leftColumn(m *Model) []string {
	listW, _, _ := m.layout()
	var out []string
	for _, l := range strings.Split(plain(m), "\n") {
		out = append(out, ansi.Cut(l, 0, listW))
	}
	return out
}

func TestDashboardKeepsListClicksAligned(t *testing.T) {
	m := fakeModel(t, 120, 36)
	if m.dashHeight() == 0 {
		t.Fatal("the summary should show at this size")
	}
	y := -1
	for i, l := range leftColumn(m) {
		if strings.Contains(l, "feature/cart") {
			y = i
			break
		}
	}
	if y < 0 {
		t.Fatal("feature/cart not on screen")
	}
	m.Update(tea.MouseClickMsg{X: 8, Y: y, Button: tea.MouseLeft})
	if r, _ := m.selected(); r.kind != rowTree || m.snap.States[r.wt].Branch != "feature/cart" {
		t.Errorf("clicking feature/cart selected %+v", r)
	}

	// Clicking the summary opens the overview.
	m.Update(tea.MouseClickMsg{X: 8, Y: 1, Button: tea.MouseLeft})
	if !m.overview {
		t.Error("clicking the summary should open the overview")
	}
}

func TestDashboardKeepsCursorOnScreen(t *testing.T) {
	m := fakeModel(t, 100, 22)
	if m.dashHeight() == 0 {
		t.Fatal("the summary should show at this size")
	}
	press(m, "G")
	if !strings.Contains(strings.Join(leftColumn(m), "\n"), "old.ts") {
		t.Errorf("the last file is selected but scrolled off the list:\n%s", plain(m))
	}
}

func TestOverview(t *testing.T) {
	m := fakeModel(t, 140, 36)
	press(m, "w")
	out := plain(m)
	for _, want := range []string{"Overview of every worktree", "WORKTREE", "feature/auth", "feature/cart", "+122 −16", "↑4 ↓1",
		"Needs attention", "src/api/client.ts is changed in feature/auth, feature/cart", "feature/auth is 1 commit behind main"} {
		if !strings.Contains(out, want) {
			t.Errorf("overview is missing %q:\n%s", want, out)
		}
	}

	// It starts on the selected worktree; enter selects the one under the
	// cursor in the list.
	press(m, "down", "enter")
	if m.overview {
		t.Fatal("enter should close the overview")
	}
	if r, _ := m.selected(); r.kind != rowTree || m.snap.States[r.wt].Branch != "feature/cart" {
		t.Errorf("enter should select feature/cart, got %+v", r)
	}

	// a goes straight to that worktree's agent.
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	press(m, "w", "up", "a")
	if m.overview || m.focus != focusChat || m.chatSession().name != "feature/auth" {
		t.Errorf("a should open feature/auth's agent, focus=%d", m.focus)
	}
	m.chatSession().pending = []pendingAction{{kind: pendingApproval, text: "npm test", tool: "Bash"}}
	press(m, "tab", "w")
	if out := plain(m); !strings.Contains(out, "feature/auth's agent is waiting for you: npm test") || !strings.Contains(out, "✦ needs you") {
		t.Errorf("the overview should show the waiting agent:\n%s", out)
	}
}

func TestPasteGoesToTheFocusedField(t *testing.T) {
	m := fakeModel(t, 120, 36)
	press(m, "/")
	m.Update(tea.PasteMsg{Content: "login"})
	if m.filterQuery != "login" || m.fileRowCount() != 1 {
		t.Errorf("a paste into the filter should filter: query %q, %d files", m.filterQuery, m.fileRowCount())
	}
	press(m, "esc")

	// A dialog without a text field ignores it rather than reading keys.
	press(m, "d")
	m.Update(tea.PasteMsg{Content: "y\n"})
	if m.confirm == nil {
		t.Error("a paste must not answer the remove dialog")
	}
}
