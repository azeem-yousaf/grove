package ui

import (
	"fmt"
	"slices"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The action palette (ctrl+k) lists what can be done with the current
// selection, with each action's key, and runs the one picked. Typing
// filters it, so nobody needs to remember which letter does what, and the
// keys shown teach them along the way.

// paletteAction is one entry of the palette.
type paletteAction struct {
	name string
	key  string
	when func(m *Model) bool // nil means always
	run  func(m *Model) tea.Cmd
}

// actionMenu is the open palette.
type actionMenu struct {
	input  textinput.Model
	shown  []paletteAction // the actions matching the input
	cursor int
	offset int // first shown action on screen
	total  int // actions available before filtering, which sets the height
}

const actionsWidth = 52

func onWorktree(m *Model) bool {
	_, ok := m.selected()
	return ok
}

func onFile(m *Model) bool {
	_, _, ok := m.selectedFile()
	return ok
}

// selectedSession is the agent session of the selected worktree, if any.
func (m *Model) selectedSession() *session {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	return m.sessionFor(m.snap.States[r.wt].Path)
}

// inGroup reports whether the cursor is on a plan group or a grouped task's
// worktree.
func inGroup(m *Model) bool {
	r, ok := m.cursorRow()
	if !ok {
		return false
	}
	if r.kind == rowGroup {
		return true
	}
	_, t := m.taskFor(m.snap.States[r.wt].Path)
	return t != nil && t.Group != ""
}

// allActions is every action in the order the palette lists them: the
// selection's first, then the rest.
func allActions(m *Model) []paletteAction {
	k := m.keys
	key := func(b interface{ Keys() []string }) string { return b.Keys()[0] }
	return []paletteAction{
		{"Open the file", "enter", onFile, (*Model).openFile},
		{"Open the worktree's agent", key(k.Agent), onWorktree, (*Model).openChat},
		{"Run a git command here", key(k.Git), onWorktree, (*Model).openGitPrompt},
		{"Open the worktree in " + m.editorName(), key(k.Editor), onWorktree, (*Model).openEditor},
		{"Show in folder", key(k.Reveal), onWorktree, (*Model).reveal},
		{"Copy the path", key(k.Copy), onWorktree, (*Model).copyPath},
		{"Commit, push, pull request or update from base", key(k.Ship), onWorktree, (*Model).openShip},
		{"Merge other branches into this worktree", key(k.Merge), onWorktree, (*Model).openConflictReview},
		{"Undo the agent's changes", key(k.Undo), func(m *Model) bool { return m.selectedSession() != nil },
			func(m *Model) tea.Cmd { return m.askUndo(m.selectedSession()) }},
		{"Open the group chat", key(k.Group), inGroup, (*Model).openSelectedGroup},
		{"Remove the worktree", key(k.Remove), onWorktree, (*Model).askRemove},
		{"New worktree", key(k.New), nil, (*Model).askCreate},
		{"Plan tasks with the planner", key(k.Planner), nil, (*Model).openPlanner},
		{"Review the planner's plan", "", func(m *Model) bool { return m.draft() != nil }, (*Model).openReview},
		{"Overview of every worktree", key(k.Overview), nil, (*Model).openOverview},
		{"Filter files", key(k.Filter), nil, (*Model).startFilter},
		{"Show only overlaps or conflicts", key(k.Overlaps), nil, (*Model).toggleOverlaps},
		{"Switch branch / uncommitted mode", key(k.Mode), nil, (*Model).toggleMode},
		{"Refresh now", key(k.Refresh), nil, (*Model).refreshNow},
		{"Keyboard shortcuts", key(k.Help), nil, func(m *Model) tea.Cmd { m.showHelp = true; return nil }},
		{"Quit", key(k.Quit), nil, (*Model).quit},
	}
}

func (m *Model) openActions() tea.Cmd {
	if m.snap == nil {
		return nil
	}
	in := textinput.New()
	in.Prompt = "› "
	in.Placeholder = "type to find an action"
	in.SetWidth(actionsWidth - 4)
	a := &actionMenu{input: in}
	m.actions = a
	a.filter(m)
	return a.input.Focus()
}

// filter lists the available actions that match the input: those whose
// name contains it first, then fuzzy matches, tightest first.
func (a *actionMenu) filter(m *Model) {
	q := strings.ToLower(strings.TrimSpace(a.input.Value()))
	var exact, loose []paletteAction
	a.total = 0
	for _, act := range allActions(m) {
		if act.when != nil && !act.when(m) {
			continue
		}
		a.total++
		name := strings.ToLower(act.name)
		switch {
		case q == "" || strings.Contains(name, q) || strings.EqualFold(act.key, q):
			exact = append(exact, act)
		case fuzzy(name, q):
			loose = append(loose, act)
		}
	}
	slices.SortStableFunc(loose, func(x, y paletteAction) int {
		return matchSpan(strings.ToLower(x.name), q) - matchSpan(strings.ToLower(y.name), q)
	})
	a.shown = append(exact, loose...)
	a.cursor, a.offset = 0, 0
}

// matchSpan is how many runes of s the shortest stretch holding q's runes
// in order takes: "ovw" takes 8 in "overview of …" but 11 in "remove the
// worktree". It assumes fuzzy(s, q).
func matchSpan(s, q string) int {
	sr, qr := []rune(s), []rune(q)
	best := len(sr) + 1
	for start, r := range sr {
		if len(qr) == 0 || r != qr[0] {
			continue
		}
		j, end := 0, start
		for i := start; i < len(sr) && j < len(qr); i++ {
			if sr[i] == qr[j] {
				j, end = j+1, i
			}
		}
		if j == len(qr) {
			best = min(best, end-start+1)
		}
	}
	return best
}

func (m *Model) onActionsKey(msg tea.KeyPressMsg) tea.Cmd {
	a := m.actions
	switch msg.String() {
	case "esc", "ctrl+k":
		m.actions = nil
		return nil
	case "ctrl+c":
		return m.quitNow()
	case "up", "ctrl+p", "shift+tab":
		if a.cursor > 0 {
			a.cursor--
		}
		return nil
	case "down", "ctrl+n", "tab":
		if a.cursor < len(a.shown)-1 {
			a.cursor++
		}
		return nil
	case "enter":
		if len(a.shown) == 0 {
			return nil
		}
		act := a.shown[a.cursor]
		m.actions = nil
		return act.run(m)
	}
	return a.update(m, msg)
}

// update passes a key or paste to the input and filters again.
func (a *actionMenu) update(m *Model, msg tea.Msg) tea.Cmd {
	before := a.input.Value()
	var cmd tea.Cmd
	a.input, cmd = a.input.Update(msg)
	if a.input.Value() != before {
		a.filter(m)
	}
	return cmd
}

func (m *Model) actionsModal() string {
	st := m.st
	a := m.actions
	inner := min(actionsWidth, m.width-8)
	a.input.SetWidth(max(inner-4, 8))
	lines := []string{
		st.bold.Foreground(st.p.accent).Render("Actions"),
		"",
		a.input.View(),
		st.faint.Render(strings.Repeat("─", inner)),
	}

	room := max(m.height-12, 3)
	if a.cursor < a.offset {
		a.offset = a.cursor
	}
	if a.cursor >= a.offset+room {
		a.offset = a.cursor - room + 1
	}
	if len(a.shown) == 0 {
		lines = append(lines, st.muted.Render("No action matches"))
	}
	for i := a.offset; i < len(a.shown) && i < a.offset+room; i++ {
		act := a.shown[i]
		keyW := ansi.StringWidth(act.key)
		name := ansi.Truncate(act.name, max(inner-keyW-4, 4), "…")
		gap := strings.Repeat(" ", max(inner-2-ansi.StringWidth(name)-keyW, 1))
		if i == a.cursor {
			line := st.accent.Render("▸ ") + st.bold.Render(name) + gap + st.key.Render(act.key)
			lines = append(lines, fill(line, inner, st.p.selBg))
			continue
		}
		lines = append(lines, "  "+st.text.Render(name)+gap+st.faint.Render(act.key))
	}
	if more := len(a.shown) - a.offset - room; more > 0 {
		lines = append(lines, st.faint.Render(fmt.Sprintf("  … %d more", more)))
	}
	// Keep the height while filtering, so the box doesn't jump about.
	for range min(a.total, room) - max(min(len(a.shown), room), 1) {
		lines = append(lines, "")
	}
	lines = append(lines, "", m.hints([][2]string{{"↑↓", "select"}, {"enter", "run"}, {"esc", "close"}}))
	return st.modal.Padding(1, 2).Render(strings.Join(lines, "\n"))
}
