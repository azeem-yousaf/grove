package ui

import (
	"fmt"
	"image/color"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/charmbracelet/x/ansi"
)

const (
	minWidth    = 50
	minHeight   = 12
	narrowWidth = 100
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m *Model) narrow() bool { return m.width < narrowWidth }

// layout returns the list pane width, the diff pane width and the height
// both panes share. In the narrow layout only one pane shows at a time, at
// full width.
func (m *Model) layout() (listW, diffW, paneH int) {
	paneH = max(m.height-2, 3)
	if m.narrow() {
		return m.width, m.width, paneH
	}
	listW = max(44, min(m.width*2/5, 72))
	return listW, m.width - listW, paneH
}

// listHeight is the number of list lines on screen.
func (m *Model) listHeight() int {
	_, _, paneH := m.layout()
	return max(paneH-m.dashHeight()-2, 1)
}

// View draws the whole screen.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.ReportFocus = true
	v.WindowTitle = "grove · " + m.repo.Name
	return v
}

func (m *Model) render() string {
	if m.width == 0 {
		return ""
	}
	if m.width < minWidth || m.height < minHeight {
		msg := lipgloss.JoinVertical(lipgloss.Center,
			m.st.bold.Render("grove needs a bigger window"),
			m.st.muted.Render(fmt.Sprintf("at least %d×%d, now %d×%d", minWidth, minHeight, m.width, m.height)))
		return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, msg)
	}

	listW, diffW, paneH := m.layout()
	right := func() string {
		if m.focus == focusChat && m.chatSession() != nil {
			return m.chatPane(diffW, paneH)
		}
		if m.focus == focusGroup && m.groupChat != nil {
			return m.groupPane(diffW, paneH)
		}
		return m.diffPane(diffW, paneH)
	}
	var body string
	switch {
	case m.usageView:
		body = m.usagePane(m.width, paneH)
	case m.overview && m.snap != nil:
		body = m.overviewPane(m.width, paneH)
	case m.narrow() && m.focus != focusList:
		body = right()
	case m.narrow():
		body = m.listPane(listW, paneH)
	default:
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.listPane(listW, paneH), right())
	}
	screen := lipgloss.JoinVertical(lipgloss.Left, m.header(), body, m.footer())

	switch {
	case m.quitAsk:
		return m.overlay(screen, m.quitModal())
	case m.confirm != nil:
		return m.overlay(screen, m.removeModal())
	case m.creating != nil:
		return m.overlay(screen, m.createModal())
	case m.picker != nil:
		return m.overlay(screen, m.pickerModal())
	case m.watchAsk != nil:
		return m.overlay(screen, m.watchModal())
	case m.undo != nil:
		return m.overlay(screen, m.undoModal())
	case m.conflicts != nil:
		return m.overlay(screen, m.conflictModal())
	case m.review != nil:
		return m.overlay(screen, m.reviewModal())
	case m.showHelp:
		return m.overlay(screen, m.helpModal())
	case m.actions != nil:
		return m.overlay(screen, m.actionsModal())
	case m.ship != nil:
		return m.overlay(screen, m.shipModal())
	case m.commit != nil:
		return m.overlay(screen, m.commitModal())
	case m.revert != nil:
		return m.overlay(screen, m.revertModal())
	}
	return screen
}

// overlay centres box on top of screen.
func (m *Model) overlay(screen, box string) string {
	x := max((m.width-lipgloss.Width(box))/2, 0)
	y := max((m.height-lipgloss.Height(box))/2, 0)
	out := lipgloss.NewCompositor(
		lipgloss.NewLayer(screen),
		lipgloss.NewLayer(box).X(x).Y(y).Z(1),
	).Render()
	// Never draw past the terminal, even if the box didn't fit.
	lines := strings.Split(out, "\n")
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > m.width {
			lines[i] = ansi.Truncate(l, m.width, "")
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) header() string {
	st := m.st
	logo := st.pillAccent.Render("grove")
	name := st.bold.Render(m.repo.Name)
	version := " " + st.faint.Render(m.version)

	mode := st.muted.Render("changes since ") + st.text.Render(m.baseLabel())
	if m.mode == git.ModeUncommitted {
		mode = st.text.Render("uncommitted") + st.muted.Render(" changes only")
	} else if m.base == "" && m.baseReady {
		mode = st.muted.Render("uncommitted changes") + st.faint.Render(" (no base branch found)")
	}

	var pills []string
	count := ""
	if m.snap != nil {
		// Overlaps predicted to conflict count as conflicts, not overlaps.
		if n := m.cleanOverlapCount(); n > 0 {
			pills = append(pills, st.pillWarn.Render(fmt.Sprintf("⚠ %s", plural(n, "overlap"))))
		}
		if n := m.conflictCount(); n > 0 {
			pills = append(pills, st.pillConflict.Render(fmt.Sprintf("✗ %s", plural(n, "conflict"))))
		}
		count = st.muted.Render(plural(len(m.snap.States), "worktree"))
	}
	// status and its short form, for when space runs out.
	status, short := "", ""
	spin := spinnerFrames[m.frame%len(spinnerFrames)]
	switch {
	case m.removing != nil:
		status, short = st.del.Render(spin)+st.muted.Render(" removing "+m.removing.st.Name()), st.del.Render(spin)
	case m.scanning:
		status, short = st.accent.Render(spin)+st.muted.Render(" scanning"), st.accent.Render(spin)
	case m.scanErr != nil:
		status, short = st.del.Render("✕ scan failed"), st.del.Render("✕")
	case !m.lastScan.IsZero() && !m.termFocused:
		status, short = st.faint.Render("◌ paused"), st.faint.Render("◌")
	case !m.lastScan.IsZero():
		status, short = st.add.Render("●")+st.muted.Render(" live · "+ago(m.now, m.lastScan)), st.add.Render("●")
	}

	// What the user compares against matters more than the version, the
	// worktree count (the list shows them) or how fresh the scan is, so
	// those go first when the line is too narrow.
	var left, right string
	for _, v := range [][3]string{
		{version, count, status},
		{version, "", status},
		{version, "", short},
		{"", "", short},
	} {
		left = " " + logo + v[0] + "  " + name + st.faint.Render("  ·  ") + mode
		parts := slices.DeleteFunc(append(slices.Clone(pills), v[1], v[2]), func(s string) bool { return s == "" })
		right = strings.Join(parts, st.faint.Render("  ·  ")) + " "
		if ansi.StringWidth(left)+ansi.StringWidth(right) < m.width {
			break
		}
	}
	return spread(left, right, m.width)
}

func (m *Model) footer() string {
	st := m.st
	if m.filterActive {
		m.filter.SetWidth(max(m.width/2, 20))
		count := ""
		if m.snap != nil {
			n := m.fileRowCount()
			word := "matches"
			if n == 1 {
				word = "match"
			}
			count = st.muted.Render(fmt.Sprintf("%d %s · ", n, word))
		}
		hint := count + m.hints([][2]string{{"enter", "done"}, {"esc", "clear"}})
		return spread(" "+m.filter.View(), hint+" ", m.width)
	}
	if m.searchActive {
		m.searchInput.SetWidth(max(m.width/2, 20))
		status := ""
		if m.searchQuery != "" {
			status = st.muted.Render(m.searchStatus() + " · ")
		}
		hint := status + m.hints([][2]string{{"↑↓", "matches"}, {"enter", "done"}, {"esc", "clear"}})
		return spread(" "+m.searchInput.View(), hint+" ", m.width)
	}
	if g := &m.gitCmd; g.active {
		label := " " + st.pillAccent.Render(g.target.Name()) + " " + st.accent.Render("git ")
		hint := m.hints([][2]string{{"enter", "run"}, {"↑", "history"}, {"esc", "cancel"}}) + " "
		g.input.SetWidth(max(m.width-lipgloss.Width(label)-lipgloss.Width(hint)-2, 10))
		return spread(label+g.input.View(), hint, m.width)
	}

	// pinned hints always show, after as many of pairs as fit: the way to
	// every other key matters most when the footer is too narrow for them.
	var pairs, pinned [][2]string
	everything := [][2]string{{"ctrl+k", "actions"}, {"?", "help"}}
	r, ok := m.selected()
	_, _, onFile := m.selectedFile()
	switch {
	case m.confirm != nil:
		pairs = [][2]string{{"enter", "confirm"}, {"esc", "cancel"}}
	case m.creating != nil:
		pairs = [][2]string{{"enter", "create"}, {"tab", "next field"}, {"esc", "cancel"}}
	case m.picker != nil, m.watchAsk != nil, m.undo != nil, m.quitAsk:
		pairs = [][2]string{{"enter", "confirm"}, {"esc", "cancel"}}
	case m.conflicts != nil, m.review != nil:
		pairs = [][2]string{{"esc", "close"}}
	case m.showHelp:
		pairs = [][2]string{{"esc", "close"}}
	case m.actions != nil:
		pairs = [][2]string{{"↑↓", "select"}, {"enter", "run"}, {"esc", "close"}}
	case m.ship != nil:
		pairs = [][2]string{{"c p r m b", "choose"}, {"esc", "close"}}
	case m.commit != nil:
		pairs = [][2]string{{"enter", "commit"}, {"tab", "files / message"}, {"esc", "cancel"}}
	case m.revert != nil:
		pairs = [][2]string{{"enter", "put it back"}, {"esc", "keep it"}}
	case m.usageView:
		pairs = [][2]string{{"↑↓", "scroll"}, {"pgup/pgdn", "page"}, {"esc", "close"}, {"q", "quit"}}
		pinned = [][2]string{{"?", "help"}}
	case m.overview:
		pairs = [][2]string{{"↑↓", "select"}, {"enter", "show in list"}, {"a", "agent"}, {"c", "merge here"}, {"T", "tokens"}, {"w", "close"}, {"r", "refresh"}, {"q", "quit"}}
		pinned = [][2]string{{"?", "help"}}
	case m.focus == focusChat:
		pairs = [][2]string{{"enter", "send"}, {"shift+enter", "new line"}, {"tab", "list"}, {"ctrl+y", "copy reply"}, {"ctrl+v", "paste"}, {"pgup/pgdn", "scroll"}, {"ctrl+x", "stop"}, {"ctrl+z", "undo"}, {"ctrl+a", "switch agent"}}
	case m.focus == focusGroup:
		pairs = [][2]string{{"enter", "post"}, {"@branch", "to one agent"}, {"tab", "list"}, {"pgup/pgdn", "scroll"}}
	case m.focus == focusDiff && (onFile || m.wtView != nil):
		next := "hunk"
		if m.searchQuery != "" {
			next = "match"
		}
		view := "whole worktree"
		if m.wtView != nil && !m.wtView.since {
			view = "last turn"
		} else if m.wtView != nil {
			view = "all changes"
		}
		pairs = [][2]string{{"↑↓", "scroll"}, {"n/N", next}, {"x", "put back"}, {">", "ask agent"}, {"/", "search"}, {"v", view}, {"[ ]", "file"}, {"tab", "list"}}
		pinned = everything
	case m.focus == focusDiff:
		// A worktree or group summary: nothing to open, no hunks.
		pairs = [][2]string{{"↑↓", "scroll"}, {"[ ]", "prev / next"}, {"tab", "list"}}
		pinned = everything
	case m.focus == focusList && m.groupSelected():
		pairs = [][2]string{{"enter", "fold"}, {"t", "group chat"}, {"[ ]", "prev / next"}, {"w", "overview"}, {"n", "new"}, {"P", "planner"}, {"o", "overlaps"}, {"/", "filter"}, {"q", "quit"}}
		pinned = everything
	case ok && r.kind == rowFile:
		pairs = [][2]string{{"enter", "open"}, {"f", "folder"}, {"y", "copy"}, {"e", "editor"}, {":", "git"}, {"tab", "diff"}, {"m", "mode"}, {"/", "filter"}, {"q", "quit"}}
		pinned = everything
	case m.snap != nil && len(m.snap.States) == 1:
		// Nothing to merge or remove yet: point at making more.
		pairs = [][2]string{{"n", "new worktree"}, {"P", "planner"}, {"a", "agent"}, {"s", "ship"}, {":", "git"}, {"e", "editor"}, {"f", "folder"}, {"m", "mode"}, {"q", "quit"}}
		pinned = everything
	default:
		pairs = [][2]string{{"enter", "fold"}, {"a", "agent"}, {"s", "ship"}, {"w", "overview"}, {":", "git"}, {"n", "new"}, {"c", "merge"}, {"e", "editor"}, {"f", "folder"}, {"d", "remove"}, {"o", "overlaps"}, {"m", "mode"}, {"/", "filter"}, {"q", "quit"}}
		pinned = everything
	}

	right := ""
	if m.toast.text != "" {
		sty, icon := st.muted, "·"
		switch m.toast.kind {
		case toastOK:
			sty, icon = st.add, "✓"
		case toastErr:
			sty, icon = st.del, "✕"
		}
		right = sty.Render(icon+" ") + st.text.Render(ansi.Truncate(m.toast.text, m.width/2, "…")) + " "
	} else if m.searchQuery != "" && m.focus == focusDiff {
		right = st.accent.Render("/"+m.searchQuery) + st.muted.Render(" "+m.searchStatus()) + st.faint.Render("  esc clears") + " "
	} else if m.filtering() {
		var parts []string
		if m.filterQuery != "" {
			parts = append(parts, st.accent.Render("/"+m.filterQuery))
		}
		switch m.overlapView {
		case overlapsShown:
			parts = append(parts, st.warn.Render("overlaps only"))
		case conflictsShown:
			parts = append(parts, st.conflict.Render("conflicts only"))
		}
		right = strings.Join(parts, " ") + st.faint.Render("  esc clears") + " "
	}

	room := m.width - lipgloss.Width(right) - 2
	tail := ""
	if len(pinned) > 0 {
		tail = st.faint.Render("  ") + m.hints(pinned)
		if lipgloss.Width(" "+tail) > room {
			tail = ""
			pairs = append(pinned, pairs...) // too narrow to keep them apart
		}
	}
	left := " "
	for i, p := range pairs {
		item := m.hints([][2]string{p})
		sep := ""
		if i > 0 {
			sep = st.faint.Render("  ")
		}
		if lipgloss.Width(left+sep+item+tail) > room {
			break
		}
		left += sep + item
	}
	if left == " " {
		tail = strings.TrimPrefix(tail, st.faint.Render("  "))
	}
	return spread(left+tail, right, m.width)
}

func (m *Model) hints(pairs [][2]string) string {
	var parts []string
	for _, p := range pairs {
		parts = append(parts, m.st.key.Render(p[0])+" "+m.st.keyDesc.Render(p[1]))
	}
	return strings.Join(parts, m.st.faint.Render("  "))
}

func (m *Model) fileRowCount() int {
	n := 0
	for _, r := range m.rows {
		if r.kind == rowFile {
			n++
		}
	}
	return n
}

// panel draws a rounded box with a title and info set into the top border
// and an optional label in the bottom border.
func (m *Model) panel(title, info, foot string, body []string, w, h int, focused bool) string {
	p := m.st.p
	bc := p.border
	titleStyle := m.st.paneTitle.Foreground(p.muted)
	if focused {
		bc = p.focus
		titleStyle = m.st.paneTitle.Foreground(p.accent)
	}
	b := lipgloss.NewStyle().Foreground(bc)
	inner := w - 2

	infoS := ""
	if info != "" {
		infoS = " " + info + " "
	}
	titleRoom := max(inner-ansi.StringWidth(infoS)-4, 4)
	titleS := " " + titleStyle.Render(ansi.Truncate(title, titleRoom, "…")) + " "
	dashes := max(w-4-ansi.StringWidth(titleS)-ansi.StringWidth(infoS), 0)

	var out strings.Builder
	out.WriteString(b.Render("╭─") + titleS + b.Render(strings.Repeat("─", dashes)) + infoS + b.Render("─╮"))
	side := b.Render("│")
	for i := 0; i < h-2; i++ {
		line := ""
		if i < len(body) {
			line = body[i]
		}
		out.WriteString("\n" + side + fill(line, inner, nil) + side)
	}
	footS := ""
	if foot != "" {
		footS = " " + m.st.faint.Render(foot) + " "
	}
	out.WriteString("\n" + b.Render("╰"+strings.Repeat("─", max(inner-1-ansi.StringWidth(footS), 0))) + footS + b.Render("─╯"))
	return out.String()
}

func (m *Model) listPane(w, h int) string {
	// The summary sits above the list; listHeight and onClick allow for it.
	if dh := m.dashHeight(); dh > 0 {
		return lipgloss.JoinVertical(lipgloss.Left, m.dashboardPane(w, dh), m.worktreesPane(w, h-dh))
	}
	return m.worktreesPane(w, h)
}

func (m *Model) worktreesPane(w, h int) string {
	inner := w - 2
	ih := h - 2
	focused := m.focus == focusList
	if m.snap == nil {
		var body []string
		for i := 0; i < ih/2-1; i++ {
			body = append(body, "")
		}
		if m.scanErr != nil {
			body = append(body,
				center(m.st.del.Render("✕ Couldn't read worktrees"), inner),
				center(m.st.muted.Render(ansi.Truncate(m.scanErr.Error(), inner-4, "…")), inner),
				"",
				center(m.hints([][2]string{{"r", "retry"}}), inner))
		} else {
			body = append(body, center(m.st.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)])+m.st.muted.Render(" Reading worktrees…"), inner))
		}
		return m.panel("Worktrees", "", "", body, w, h, focused)
	}

	lines := m.listLines(inner)
	end := min(m.listOffset+ih, len(lines))
	body := make([]string, 0, ih)
	for _, l := range lines[m.listOffset:end] {
		body = append(body, l.text)
	}

	info := ""
	if n := m.totalFiles(); n > 0 {
		a, d := m.totals()
		info = m.st.muted.Render(plural(n, "file")) + "  " + m.st.add.Render(fmt.Sprintf("+%s", compact(a))) + " " + m.st.del.Render(fmt.Sprintf("−%s", compact(d)))
	}
	foot := ""
	if len(lines) > ih {
		foot = scrollLabel(m.listOffset, len(lines), ih)
	}
	return m.panel("Worktrees", info, foot, body, w, h, focused)
}

func (m *Model) totalFiles() int {
	n := 0
	for _, s := range m.snap.States {
		n += len(s.Files)
	}
	return n
}

func (m *Model) totals() (added, deleted int) {
	for _, s := range m.snap.States {
		added += s.Added
		deleted += s.Deleted
	}
	return added, deleted
}

func scrollLabel(offset, total, height int) string {
	switch {
	case offset == 0:
		return "top"
	case offset+height >= total:
		return "end"
	}
	return fmt.Sprintf("%d%%", offset*100/max(total-height, 1))
}

// diffHeader is the fixed area above the diff: which worktree the file is in,
// what kind of change it is, and an overlap warning when other worktrees
// touched the same file.
func (m *Model) diffHeader(width int) []string {
	st := m.st
	s, f, ok := m.selectedFile()
	if !ok {
		return nil
	}
	parts := []string{st.bold.Render(s.Name()), statusStyle(st, f.Status).Render(f.Status.Label())}
	if f.Uncommitted && m.snap.Mode == git.ModeBranch {
		parts = append(parts, st.warn.Render("● uncommitted"))
	}
	opener := "default app"
	if cmd, ok := m.opener.Overrides[strings.ToLower(filepath.Ext(f.Path))]; ok && cmd != "default" {
		opener = cmd
	}
	right := m.hints([][2]string{{"enter", "open in " + opener}})
	lines := []string{spread(" "+strings.Join(parts, st.faint.Render(" · ")), right+" ", width)}

	if others := m.snap.Overlaps[f.Path]; len(others) > 1 {
		r, _ := m.selected()
		conflicts := m.conflictsFor(r.wt, f.Path)
		var clash, also []string
		for _, i := range others {
			o := m.snap.States[i]
			if o.Path == s.Path {
				continue
			}
			for _, of := range o.Files {
				if of.Path != f.Path {
					continue
				}
				if slices.Contains(conflicts, i) {
					clash = append(clash, st.conflict.Render(o.Name()))
				} else {
					also = append(also, st.bold.Foreground(st.p.warn).Render(o.Name())+st.faint.Render(fmt.Sprintf(" +%d −%d", of.Added, of.Deleted)))
				}
			}
		}
		var parts []string
		if len(clash) > 0 {
			parts = append(parts, st.conflict.Render("✗ Conflicts with ")+strings.Join(clash, st.faint.Render(", ")))
		}
		if len(also) > 0 {
			label := "⚠ Also changed in "
			if len(clash) > 0 {
				label = "⚠ also in "
			}
			parts = append(parts, st.warn.Bold(true).Render(label)+strings.Join(also, st.faint.Render(", ")))
		}
		bg := st.p.selBgDim
		if len(clash) > 0 {
			bg = st.p.conflictBg
		}
		banner := " " + strings.Join(parts, st.faint.Render("  ·  "))
		lines = append(lines, fill(ansi.Truncate(banner, width, "…"), width, bg))
	}
	return append(lines, st.faint.Render(strings.Repeat("─", width)))
}

func (m *Model) diffPane(w, h int) string {
	focused := m.focus == focusDiff
	inner := w - 2
	if m.wtView != nil {
		foot := ""
		if m.diff.TotalLineCount() > m.diff.Height() {
			foot = scrollLabel(m.diff.YOffset(), m.diff.TotalLineCount(), m.diff.Height())
		}
		if m.parsed.truncated {
			foot = fmt.Sprintf("first %d lines · %s", maxWorktreeLines, foot)
		}
		return m.panel(m.wtView.name, m.wtTitle(), foot, strings.Split(m.diff.View(), "\n"), w, h, focused)
	}
	if cr, ok := m.cursorRow(); ok && cr.kind == rowGroup {
		_, grp := m.groupPlan(cr.grp)
		foot := ""
		if m.diff.TotalLineCount() > m.diff.Height() {
			foot = scrollLabel(m.diff.YOffset(), m.diff.TotalLineCount(), m.diff.Height())
		}
		return m.panel(groupTitle(grp), m.st.faint.Render("group"), foot, strings.Split(m.diff.View(), "\n"), w, h, focused)
	}
	r, ok := m.selected()
	if !ok {
		body := []string{}
		for i := 0; i < (h-2)/2-1; i++ {
			body = append(body, "")
		}
		body = append(body, center(m.st.faint.Render("Select a worktree or file"), inner))
		return m.panel("Details", "", "", body, w, h, focused)
	}
	s := m.snap.States[r.wt]
	if r.kind == rowTree {
		foot := ""
		if m.diff.TotalLineCount() > m.diff.Height() {
			foot = scrollLabel(m.diff.YOffset(), m.diff.TotalLineCount(), m.diff.Height())
		}
		return m.panel(s.Name(), m.st.faint.Render("worktree"), foot, strings.Split(m.diff.View(), "\n"), w, h, focused)
	}

	f := s.Files[r.file]
	title := f.Path
	if f.OldPath != "" {
		title = f.OldPath + " → " + f.Path
	}
	info := m.st.add.Render(fmt.Sprintf("+%d", f.Added)) + " " + m.st.del.Render(fmt.Sprintf("−%d", f.Deleted))
	if f.Binary {
		info = m.st.faint.Render("binary")
	}
	body := m.diffHeader(inner)
	body = append(body, strings.Split(m.diff.View(), "\n")...)
	foot := ""
	if m.diff.TotalLineCount() > m.diff.Height() {
		foot = scrollLabel(m.diff.YOffset(), m.diff.TotalLineCount(), m.diff.Height())
	}
	if m.parsed.truncated && m.shownKey == m.diffWanted {
		foot = fmt.Sprintf("first %d lines · %s", maxDiffLines, foot)
	}
	return m.panel(title, info, foot, body, w, h, focused)
}

// refreshDiffView puts the right content into the diff viewport when the
// selection, the loaded diff, the size or the theme changed.
func (m *Model) refreshDiffView() {
	if m.snap == nil || m.width == 0 {
		return
	}
	_, diffW, paneH := m.layout()
	inner := diffW - 2
	if m.wtView != nil {
		m.refreshWorktreeView(inner, paneH)
		return
	}
	if cr, ok := m.cursorRow(); ok && cr.kind == rowGroup {
		m.diff.SetWidth(inner)
		m.diff.SetHeight(max(paneH-2, 1))
		// Redrawn every second: agents and the chat change all the time.
		key := m.listGroups[cr.grp].key()
		want := fmt.Sprintf("group|%s|%d|%t|%p|%p|%d", key, inner, m.dark, m.snap, m.predictions, m.now.Unix())
		if want == m.renderedFor {
			return
		}
		same := strings.HasPrefix(m.renderedFor, "group|"+key+"|")
		m.renderedFor = want
		m.shownKey = ""
		m.diff.LeftGutterFunc = nil
		m.diff.SetContentLines(m.groupSummaryLines(cr.grp, inner))
		if !same {
			m.diff.GotoTop()
		}
		return
	}
	r, ok := m.selected()
	if !ok {
		return
	}
	headerH := len(m.diffHeader(inner))
	m.diff.SetWidth(inner)
	m.diff.SetHeight(max(paneH-2-headerH, 1))

	if r.kind == rowTree {
		s := m.snap.States[r.wt]
		// Redrawn every second: it shows the worktree's agent.
		want := fmt.Sprintf("tree|%s|%d|%t|%p|%p|%d", s.Path, inner, m.dark, m.snap, m.predictions, m.now.Unix())
		if want == m.renderedFor {
			return
		}
		sameTree := strings.HasPrefix(m.renderedFor, "tree|"+s.Path+"|")
		m.renderedFor = want
		m.shownKey = ""
		m.diff.LeftGutterFunc = nil
		m.diff.SetContentLines(m.summaryLines(r.wt, inner))
		if !sameTree {
			m.diff.GotoTop()
		}
		return
	}

	k := m.diffWanted
	raw, have := m.diffCache[k]
	err, failed := m.diffErrs[k]
	if !have && !failed {
		// Still loading: keep showing the previous file's diff rather than
		// flashing an empty pane for a few milliseconds.
		if m.shownKey == "" && m.renderedFor != "loading" {
			m.renderedFor = "loading"
			m.diff.LeftGutterFunc = nil
			m.diff.SetContentLines(m.placeholder("", "Loading diff…", inner))
		}
		return
	}
	_, f, _ := m.selectedFile()
	// Predictions arriving change the highlighted hunks, so they are part of
	// the key.
	clash := m.conflictLines(r.wt, f.Path)
	want := fmt.Sprintf("diff|%s|%d|%t|%d|%v|%s", k, inner, m.dark, len(raw), clash, m.searchQuery)
	if want == m.renderedFor && raw == m.shownRaw {
		return
	}
	sameFile := k == m.shownKey
	m.renderedFor = want
	m.shownKey, m.shownRaw = k, raw

	switch pd := parseDiff(raw); {
	case failed:
		m.parsed = parsedDiff{}
		m.diff.LeftGutterFunc = nil
		m.diff.SetContentLines(m.placeholder("✕", "Couldn't load the diff\n"+err.Error(), inner))
	case pd.binary || f.Binary:
		m.parsed = parsedDiff{}
		m.diff.LeftGutterFunc = nil
		m.diff.SetContentLines(m.placeholder("◇", "Binary file\nPress enter to open it in its default app", inner))
	case len(pd.lines) == 0:
		m.parsed = parsedDiff{}
		m.diff.LeftGutterFunc = nil
		msg := "No line changes\nOnly the file mode or attributes changed"
		if f.Status == git.Untracked || f.Status == git.Added {
			msg = "Empty file"
		}
		m.diff.SetContentLines(m.placeholder("○", msg, inner))
	default:
		m.parsed = pd
		content, gutter := renderDiff(pd, f.Path, inner, m.st, clash, m.searchQuery)
		m.diff.LeftGutterFunc = m.diffGutter(gutter)
		m.diff.SetContentLines(content)
	}
	if !sameFile {
		m.curHunk = -1
		m.diff.GotoTop()
		m.diff.SetXOffset(0)
	}
}

// placeholder centres a short message in the diff area.
func (m *Model) placeholder(icon, msg string, width int) []string {
	var out []string
	for i := 0; i < m.diff.Height()/2-2; i++ {
		out = append(out, "")
	}
	lines := strings.Split(msg, "\n")
	if icon != "" {
		out = append(out, center(m.st.accent.Render(icon), width))
	}
	for i, l := range lines {
		s := m.st.muted
		if i == 0 {
			s = m.st.bold
		}
		out = append(out, center(s.Render(ansi.Truncate(l, width-2, "…")), width))
	}
	return out
}

// summaryLines describe a worktree when its header row is selected.
func (m *Model) summaryLines(wt, width int) []string {
	st := m.st
	s := m.snap.States[wt]
	label := func(l string) string { return "  " + st.muted.Render(fmt.Sprintf("%-16s", l)) }
	out := []string{""}

	title := "  " + st.bold.Foreground(st.p.accent).Render(s.Name())
	if wt == m.hereIdx {
		title += "  " + st.accent.Render("◆ you are here")
	}
	out = append(out, title, "  "+st.faint.Render(shortPath(s.Path, width-4)), "")

	if s.Err != nil {
		return append(out, "  "+st.del.Render("✕ "+s.Err.Error()))
	}

	switch {
	case s.Base == "":
		out = append(out, label("Compared with")+st.text.Render("HEAD")+st.faint.Render("  (no base branch found)"))
	case m.snap.Mode == git.ModeUncommitted:
		out = append(out, label("Showing")+st.text.Render("uncommitted work only"))
	default:
		mb := ""
		if len(s.Rev) >= 7 {
			mb = st.faint.Render("  from " + s.Rev[:7])
		}
		out = append(out, label("Compared with")+st.text.Render(s.Base)+mb)
	}
	if s.Base != "" && s.Branch != s.Base {
		ab := st.accent.Render(fmt.Sprintf("↑ %d ahead", s.Ahead)) + st.faint.Render("   ") + st.warn.Render(fmt.Sprintf("↓ %d behind", s.Behind))
		if s.Behind > 0 {
			ab += st.faint.Render("  · s to catch up")
		}
		out = append(out, label("Commits")+ab)
	}
	if len(s.Files) == 0 {
		out = append(out, label("Changes")+st.add.Render("✓ ")+st.muted.Render("none"))
	} else {
		out = append(out, label("Changes")+st.text.Render(plural(len(s.Files), "file"))+"  "+st.add.Render(fmt.Sprintf("+%d", s.Added))+" "+st.del.Render(fmt.Sprintf("−%d", s.Deleted)))
		counts := map[git.Status]int{}
		uncommitted := 0
		for _, f := range s.Files {
			counts[f.Status]++
			if f.Uncommitted {
				uncommitted++
			}
		}
		var kinds []string
		for _, k := range []git.Status{git.Modified, git.Added, git.Untracked, git.Renamed, git.Deleted, git.Conflicted, git.Copied, git.TypeChanged} {
			if counts[k] > 0 {
				kinds = append(kinds, statusStyle(st, k).Render(fmt.Sprintf("%d %s", counts[k], k.Label())))
			}
		}
		out = append(out, label("")+strings.Join(kinds, st.faint.Render(" · ")))
		if m.snap.Mode == git.ModeBranch && uncommitted > 0 {
			out = append(out, label("")+st.warn.Render(fmt.Sprintf("● %d not committed yet", uncommitted)))
		}
	}
	if pr, ok := m.prs[s.Branch]; ok && s.Branch != "" {
		out = append(out, label("Pull request")+prStyle(st, pr).Render(pr.Label())+st.faint.Render("  "+pr.URL))
	}
	out = append(out, m.taskLines(s, width)...)
	out = append(out, m.agentLines(s, width)...)
	out = append(out, m.commitLines(s, width)...)

	// Files this worktree shares with others: the likely merge conflicts.
	var shared []string
	clashes := 0
	for _, f := range s.Files {
		idx := m.snap.Overlaps[f.Path]
		if len(idx) < 2 {
			continue
		}
		var names []string
		for _, i := range idx {
			if i != wt {
				names = append(names, m.snap.States[i].Name())
			}
		}
		line := "    " + st.text.Render(ansi.Truncate(f.Path, width/2, "…")) + st.faint.Render("  also in ") + st.warn.Render(strings.Join(names, ", "))
		if c := m.conflictsFor(wt, f.Path); len(c) > 0 {
			clashes++
			var with []string
			for _, i := range c {
				with = append(with, m.snap.States[i].Name())
			}
			line = "  " + st.conflict.Render("✗ ") + st.text.Render(ansi.Truncate(f.Path, width/2, "…")) + st.faint.Render("  conflicts with ") + st.conflict.Render(strings.Join(with, ", "))
		}
		shared = append(shared, line)
	}
	if len(shared) > 0 {
		out = append(out, "", "  "+st.warn.Bold(true).Render(fmt.Sprintf("⚠ %s also changed in other worktrees", plural(len(shared), "file"))))
		if clashes > 0 {
			out = append(out, "  "+st.conflict.Render(fmt.Sprintf("✗ %s will conflict when merged", plural(clashes, "file"))))
		} else {
			out = append(out, "  "+st.faint.Render("These are where merge conflicts will come from."))
		}
		out = append(out, shared...)
	}

	acts := [][2]string{{"a", "agent"}}
	if ses := m.sessionFor(s.Path); ses != nil && ses.checkpoint != "" {
		acts = append(acts, [2]string{"u", "undo agent"})
	}
	acts = append(acts, [][2]string{{":", "run git"}, {"e", "open in " + m.editorName()}, {"f", "folder"}, {"y", "copy path"}}...)
	if !s.Main && !s.Bare {
		acts = append(acts, [2]string{"d", "remove"})
	}
	// As many as fit, rather than one cut in half.
	for len(acts) > 1 && ansi.StringWidth(m.hints(acts)) > width-3 {
		acts = acts[:len(acts)-1]
	}
	out = append(out, "", "  "+m.hints(acts))

	if len(m.snap.States) == 1 {
		out = append(out, "", "  "+st.bold.Render("Work on several features side by side"))
		intro := "Each worktree is its own folder and branch, so switching between features never needs stashing, " +
			"and each can have its own agent working on it while you review the others. grove shows what every " +
			"worktree changed and warns when two touch the same file."
		for _, l := range strings.Split(ansi.Wrap(intro, max(width-4, 10), ""), "\n") {
			out = append(out, "  "+st.muted.Render(l))
		}
		out = append(out, "",
			"    "+m.hints([][2]string{{"n", "create a worktree"}, {"P", "plan a feature with the planner"}}))
	}
	return out
}

// summaryLabel starts a line of the worktree summary with a label column.
func (m *Model) summaryLabel(l string) string {
	return "  " + m.st.muted.Render(fmt.Sprintf("%-16s", l))
}

// summaryText wraps text into the summary's value column, at most most
// lines, the last ending in … when some was left out.
func (m *Model) summaryText(text string, width, most int, style lipgloss.Style) []string {
	room := max(width-20, 10)
	var lines []string
	for _, l := range renderChatText(text, room, m.st, style) {
		if strings.TrimSpace(ansi.Strip(l)) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > most {
		lines = lines[:most]
		last := lines[most-1]
		end := min(ansi.StringWidth(strings.TrimRight(ansi.Strip(last), " ")), room-1)
		lines[most-1] = ansi.Truncate(last, end, "") + m.st.faint.Render("…")
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = m.summaryLabel("") + l
	}
	return out
}

// taskLines show the plan task a worktree was made for, if any.
func (m *Model) taskLines(s git.State, width int) []string {
	p, t := m.taskFor(s.Path)
	if t == nil {
		return nil
	}
	st := m.st
	head := m.taskIcon(t) + " " + st.text.Render(t.Title) + st.faint.Render("  "+string(t.Status)+" · "+p.Title)
	out := []string{"", m.summaryLabel("Task") + ansi.Truncate(head, max(width-20, 10), "…")}
	if t.Status == plan.Failed && t.Error != "" {
		return append(out, m.summaryText(t.Error, width, 2, st.del)...)
	}
	return append(out, m.summaryText(t.Brief, width, 2, st.faint)...)
}

// agentLines show what the worktree's agent is doing and the start of its
// latest reply.
func (m *Model) agentLines(s git.State, width int) []string {
	ses := m.sessionFor(s.Path)
	if ses == nil {
		return nil
	}
	st := m.st
	var state string
	switch {
	case ses.running:
		state = st.accent.Render("✦ working")
	case len(ses.pending) > 0:
		state = st.warn.Bold(true).Render("✦ needs you")
	case ses.failed:
		state = st.del.Render("✦ stopped with an error")
	case !ses.seen:
		state = st.add.Render("✦ finished") + st.faint.Render(" · not looked at yet")
	default:
		state = st.muted.Render("idle")
	}
	out := []string{"", m.summaryLabel("Agent") + st.text.Render(ses.kind.Label()) + st.faint.Render(" · ") + state}
	if u := m.agentUsageCost(ses); len(u) > 0 {
		out = append(out, m.summaryLabel("Usage")+ansi.Truncate(strings.Join(u, st.faint.Render(" · ")), max(width-20, 10), "…"))
	}
	for i := len(ses.entries) - 1; i >= 0; i-- {
		if ses.entries[i].role == roleAgent {
			return append(out, m.summaryText(ses.entries[i].text, width, 3, st.muted)...)
		}
	}
	return out
}

// commitLines list the newest commits, marking those not in the base
// branch yet with ↑.
func (m *Model) commitLines(s git.State, width int) []string {
	commits := s.Recent
	if len(commits) == 0 && s.Last != nil {
		commits = []git.Commit{*s.Last}
	}
	if len(commits) == 0 {
		return nil
	}
	st := m.st
	out := []string{"", m.summaryLabel("Recent commits")}
	out[1] = strings.TrimRight(out[1], " ")
	for i, c := range commits {
		ahead := s.Base != "" && s.Branch != s.Base && i < s.Ahead
		mark, hash, subject := "  ", st.faint.Render(c.Hash), st.muted.Render(c.Subject)
		if ahead {
			mark, hash, subject = st.accent.Render("↑ "), st.accent.Render(c.Hash), st.text.Render(c.Subject)
		}
		when := st.faint.Render(ago(m.now, c.When))
		left := "    " + mark + hash + " " + subject
		room := width - ansi.StringWidth(when) - 3
		out = append(out, spread(ansi.Truncate(left, room, "…"), when+" ", width))
	}
	return out
}

func (m *Model) editorName() string {
	if m.opener.Editor == "" {
		return "file manager"
	}
	return strings.TrimSuffix(filepath.Base(m.opener.Editor), filepath.Ext(m.opener.Editor))
}

// helpModal builds the keyboard overlay, picking the richest layout that
// fits the terminal.
func (m *Model) helpModal() string {
	st := m.st
	col := func(sec helpSection) string {
		lines := []string{st.bold.Foreground(st.p.accent).Render(sec.title)}
		for _, k := range sec.keys {
			lines = append(lines, st.key.Render(fmt.Sprintf("%-9s", k[0]))+st.keyDesc.Render(k[1]))
		}
		return lipgloss.NewStyle().Width(30).Render(strings.Join(lines, "\n"))
	}
	var cols []string
	for _, sec := range helpSections {
		cols = append(cols, col(sec))
	}
	// grid lays the sections out perRow to a row.
	grid := func(perRow int) string {
		var rows []string
		for i := 0; i < len(cols); i += perRow {
			if i > 0 {
				rows = append(rows, "")
			}
			rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, cols[i:min(i+perRow, len(cols))]...))
		}
		return lipgloss.JoinVertical(lipgloss.Left, rows...)
	}

	legend := strings.Join([]string{
		st.warn.Render("●") + st.muted.Render(" uncommitted edits   ") +
			st.warn.Bold(true).Render("⚠") + st.muted.Render(" also changed in another worktree"),
		st.conflict.Render("✗") + st.muted.Render(" will conflict when merged with another worktree"),
		st.accent.Render("↑") + st.muted.Render(" ahead of base       ") +
			st.warn.Render("↓") + st.muted.Render(" behind base   ") +
			st.add.Render("■■") + st.del.Render("■") + st.faint.Render("··") + st.muted.Render(" size of change"),
	}, "\n")

	title := st.pillAccent.Render("grove") + "  " + st.bold.Render("Keyboard shortcuts")
	foot := st.faint.Render("grove " + m.version + "  ·  press any key to close")

	variants := []struct {
		grid   string
		legend bool
		padY   int
	}{
		{grid(len(cols)), true, 1}, {grid(3), true, 1}, {grid(3), true, 0}, {grid(3), false, 0},
		{grid(2), true, 0}, {grid(2), false, 0},
	}
	var box string
	for _, v := range variants {
		parts := []string{title, "", v.grid, ""}
		if v.legend {
			parts = append(parts, legend, "")
		}
		parts = append(parts, foot)
		box = st.modal.Padding(v.padY, 3).Render(strings.Join(parts, "\n"))
		if lipgloss.Width(box) <= m.width-2 && lipgloss.Height(box) <= m.height-2 {
			return box
		}
	}
	return box
}

// fill truncates or pads s to exactly width cells. With a background colour
// it re-applies the background after every reset inside s, so styled
// segments don't punch holes in a highlighted row.
func fill(s string, width int, bg color.Color) string {
	if ansi.StringWidth(s) > width {
		s = ansi.Truncate(s, width, "…")
	}
	pad := strings.Repeat(" ", max(width-ansi.StringWidth(s), 0))
	if bg == nil {
		return s + pad
	}
	seq := ansi.NewStyle().BackgroundColor(bg).String()
	s = strings.ReplaceAll(s, "\x1b[0m", "\x1b[m")
	s = strings.ReplaceAll(s, ansi.ResetStyle, ansi.ResetStyle+seq)
	return seq + s + pad + ansi.ResetStyle
}

// spread places left and right at the edges of width, trimming left when
// they don't both fit.
func spread(left, right string, width int) string {
	rw := ansi.StringWidth(right)
	if ansi.StringWidth(left)+rw >= width {
		left = ansi.Truncate(left, max(width-rw-1, 0), "…")
	}
	gap := max(width-ansi.StringWidth(left)-rw, 0)
	return left + strings.Repeat(" ", gap) + right
}

func center(s string, width int) string {
	pad := max((width-ansi.StringWidth(s))/2, 0)
	return strings.Repeat(" ", pad) + s
}

// ago formats a time relative to now: "just now", "4m ago", "3d ago".
func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 60*24*time.Hour:
		return fmt.Sprintf("%dw ago", int(d.Hours()/24/7))
	}
	return t.Format("Jan 2, 2006")
}
