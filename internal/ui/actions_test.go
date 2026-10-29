package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func shownNames(m *Model) []string {
	var names []string
	for _, a := range m.actions.shown {
		names = append(names, a.name)
	}
	return names
}

func TestActionsPalette(t *testing.T) {
	m := fakeModel(t, 140, 36)
	press(m, "ctrl+k") // feature/auth is selected at start
	if m.actions == nil {
		t.Fatal("ctrl+k didn't open the palette")
	}
	out := plain(m)
	for _, want := range []string{"Actions", "Open the worktree's agent", "Remove the worktree", "New worktree"} {
		if !strings.Contains(out, want) {
			t.Errorf("palette is missing %q:\n%s", want, out)
		}
	}
	names := strings.Join(shownNames(m), "|")
	// No agent has worked here, there's no file selected, no plan, no group.
	for _, absent := range []string{"Undo the agent's changes", "Open the file", "Review the planner's plan", "Open the group chat"} {
		if strings.Contains(names, absent) {
			t.Errorf("%q offered without reason: %s", absent, names)
		}
	}

	// Typing filters; names containing the text come first.
	typeText(m, "merge")
	if got := shownNames(m); len(got) == 0 || got[0] != "Merge other branches into this worktree" {
		t.Fatalf("after typing merge: %v", got)
	}
	press(m, "enter")
	if m.actions != nil || m.conflicts == nil || m.conflicts.name != "feature/auth" {
		t.Fatalf("enter didn't run the merge review: actions=%v conflicts=%+v", m.actions, m.conflicts)
	}
	press(m, "esc")

	// Fuzzy matches, and esc closes without doing anything.
	press(m, "ctrl+k")
	typeText(m, "ovw")
	if got := shownNames(m); len(got) == 0 || got[0] != "Overview of every worktree" {
		t.Fatalf("fuzzy ovw: %v", got)
	}
	press(m, "esc")
	if m.actions != nil || m.overview {
		t.Fatal("esc didn't just close the palette")
	}

	// Pasting goes into the filter.
	press(m, "ctrl+k")
	m.Update(tea.PasteMsg{Content: "quit"})
	if got := shownNames(m); len(got) != 1 || got[0] != "Quit" {
		t.Fatalf("after pasting quit: %v", got)
	}

	// Moving down then running picks the second action.
	press(m, "esc", "ctrl+k", "down", "enter")
	if !m.gitCmd.active {
		t.Errorf("second action (run git) didn't run")
	}
}

func TestFooterKeepsHelp(t *testing.T) {
	for _, w := range []int{60, 90, 140} {
		m := fakeModel(t, w, 30)
		lines := strings.Split(plain(m), "\n")
		foot := lines[len(lines)-1]
		for _, want := range []string{"? help", "ctrl+k actions"} {
			if !strings.Contains(foot, want) {
				t.Errorf("width %d: footer lost %q: %q", w, want, foot)
			}
		}
	}
	// A worktree summary in the diff pane has no hunks to jump to.
	m := fakeModel(t, 140, 30)
	press(m, "tab")
	lines := strings.Split(plain(m), "\n")
	if foot := lines[len(lines)-1]; strings.Contains(foot, "hunk") {
		t.Errorf("summary footer offers hunks: %q", foot)
	}
}

func TestHelpClosesOnAnyKey(t *testing.T) {
	m := fakeModel(t, 140, 36)
	press(m, "?", "x")
	if m.showHelp {
		t.Error("help still open after x")
	}
}
