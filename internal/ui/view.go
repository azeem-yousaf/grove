package ui

import (
	"fmt"
	"image/color"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/git"
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

func (m *Model) listHeight() int {
	_, _, paneH := m.layout()
	return max(paneH-2, 1)
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
	var body string
	switch {
	case m.narrow() && m.focus == focusDiff:
		body = m.diffPane(diffW, paneH)
	case m.narrow():
		body = m.listPane(listW, paneH)
	default:
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.listPane(listW, paneH), m.diffPane(diffW, paneH))
	}
	screen := lipgloss.JoinVertical(lipgloss.Left, m.header(), body, m.footer())

	switch {
	case m.confirm != nil:
		return m.overlay(screen, m.removeModal())
	case m.creating != nil:
		return m.overlay(screen, m.createModal())
	case m.showHelp:
		return m.overlay(screen, m.helpModal())
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
	left := " " + logo + " " + st.bold.Render(m.repo.Name)

	mode := st.muted.Render("changes since ") + st.text.Render(m.baseLabel())
	if m.mode == git.ModeUncommitted {
		mode = st.text.Render("uncommitted") + st.muted.Render(" changes only")
	} else if m.base == "" && m.baseReady {
		mode = st.muted.Render("uncommitted changes") + st.faint.Render(" (no base branch found)")
	}
	left += st.faint.Render("  ·  ") + mode

	var right []string
	if m.snap != nil {
		if n := m.snap.OverlapCount(); n > 0 {
			right = append(right, st.pillWarn.Render(fmt.Sprintf("⚠ %s", plural(n, "overlap"))))
		}
		right = append(right, st.muted.Render(plural(len(m.snap.States), "worktree")))
	}
	switch {
	case m.removing != nil:
		right = append(right, st.del.Render(spinnerFrames[m.frame%len(spinnerFrames)])+st.muted.Render(" removing "+m.removing.st.Name()))
	case m.scanning:
		right = append(right, st.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)])+st.muted.Render(" scanning"))
	case m.scanErr != nil:
		right = append(right, st.del.Render("✕ scan failed"))
	case !m.lastScan.IsZero() && !m.termFocused:
		right = append(right, st.faint.Render("◌ paused"))
	case !m.lastScan.IsZero():
		right = append(right, st.add.Render("●")+st.muted.Render(" live · "+ago(m.now, m.lastScan)))
	}
	return spread(left, strings.Join(right, st.faint.Render("  ·  "))+" ", m.width)
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
	if g := &m.gitCmd; g.active {
		label := " " + st.pillAccent.Render(g.target.Name()) + " " + st.accent.Render("git ")
		hint := m.hints([][2]string{{"enter", "run"}, {"↑", "history"}, {"esc", "cancel"}}) + " "
		g.input.SetWidth(max(m.width-lipgloss.Width(label)-lipgloss.Width(hint)-2, 10))
		return spread(label+g.input.View(), hint, m.width)
	}

	var pairs [][2]string
	r, ok := m.selected()
	switch {
	case m.confirm != nil:
		pairs = [][2]string{{"enter", "confirm"}, {"esc", "cancel"}}
	case m.creating != nil:
		pairs = [][2]string{{"enter", "create"}, {"tab", "next field"}, {"esc", "cancel"}}
	case m.showHelp:
		pairs = [][2]string{{"esc", "close"}}
	case m.focus == focusDiff:
		pairs = [][2]string{{"↑↓", "scroll"}, {"n/N", "hunk"}, {"[ ]", "file"}, {"enter", "open"}, {"tab", "list"}, {"?", "help"}}
	case ok && r.kind == rowFile:
		pairs = [][2]string{{"enter", "open"}, {"f", "folder"}, {"y", "copy"}, {"e", "editor"}, {":", "git"}, {"tab", "diff"}, {"m", "mode"}, {"/", "filter"}, {"?", "help"}, {"q", "quit"}}
	default:
		pairs = [][2]string{{"enter", "fold"}, {":", "git"}, {"n", "new"}, {"e", "editor"}, {"f", "folder"}, {"d", "remove"}, {"o", "overlaps"}, {"m", "mode"}, {"/", "filter"}, {"?", "help"}, {"q", "quit"}}
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
	} else if m.filtering() {
		var parts []string
		if m.filterQuery != "" {
			parts = append(parts, st.accent.Render("/"+m.filterQuery))
		}
		if m.overlapsOnly {
			parts = append(parts, st.warn.Render("overlaps only"))
		}
		right = strings.Join(parts, " ") + st.faint.Render("  esc clears") + " "
	}

	room := m.width - lipgloss.Width(right) - 2
	left := " "
	for i, p := range pairs {
		item := m.hints([][2]string{p})
		sep := ""
		if i > 0 {
			sep = st.faint.Render("  ")
		}
		if lipgloss.Width(left+sep+item) > room {
			break
		}
		left += sep + item
	}
	return spread(left, right, m.width)
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
		var names []string
		for _, i := range others {
			o := m.snap.States[i]
			if o.Path == s.Path {
				continue
			}
			for _, of := range o.Files {
				if of.Path == f.Path {
					names = append(names, st.bold.Foreground(st.p.warn).Render(o.Name())+st.faint.Render(fmt.Sprintf(" +%d −%d", of.Added, of.Deleted)))
				}
			}
		}
		banner := " " + st.warn.Bold(true).Render("⚠ Also changed in ") + strings.Join(names, st.faint.Render(", "))
		lines = append(lines, fill(ansi.Truncate(banner, width, "…"), width, st.p.selBgDim))
	}
	return append(lines, st.faint.Render(strings.Repeat("─", width)))
}

func (m *Model) diffPane(w, h int) string {
	focused := m.focus == focusDiff
	inner := w - 2
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
	r, ok := m.selected()
	if !ok {
		return
	}
	headerH := len(m.diffHeader(inner))
	m.diff.SetWidth(inner)
	m.diff.SetHeight(max(paneH-2-headerH, 1))

	if r.kind == rowTree {
		s := m.snap.States[r.wt]
		want := fmt.Sprintf("tree|%s|%d|%t|%p|%d", s.Path, inner, m.dark, m.snap, m.now.Unix()/30)
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
	want := fmt.Sprintf("diff|%s|%d|%t|%d", k, inner, m.dark, len(raw))
	if want == m.renderedFor && raw == m.shownRaw {
		return
	}
	sameFile := k == m.shownKey
	m.renderedFor = want
	m.shownKey, m.shownRaw = k, raw
	_, f, _ := m.selectedFile()

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
		content, gutter := renderDiff(pd, f.Path, inner, m.st)
		blank := strings.Repeat(" ", ansi.StringWidth(gutter[0]))
		m.diff.LeftGutterFunc = func(c viewport.GutterContext) string {
			if c.Index < len(gutter) && !c.Soft {
				return gutter[c.Index]
			}
			return blank
		}
		m.diff.SetContentLines(content)
	}
	if !sameFile {
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
			ab += st.faint.Render("  · rebase to catch up")
		}
		out = append(out, label("Commits")+ab)
	}
	if c := s.Last; c != nil {
		line := st.accent.Render(c.Hash) + " " + st.text.Render(c.Subject)
		out = append(out, label("Last commit")+ansi.Truncate(line, width-18, "…"))
		out = append(out, label("")+st.faint.Render(c.Author+" · "+ago(m.now, c.When)))
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

	// Files this worktree shares with others: the likely merge conflicts.
	var shared []string
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
		shared = append(shared, "    "+st.text.Render(ansi.Truncate(f.Path, width/2, "…"))+st.faint.Render("  also in ")+st.warn.Render(strings.Join(names, ", ")))
	}
	if len(shared) > 0 {
		out = append(out, "", "  "+st.warn.Bold(true).Render(fmt.Sprintf("⚠ %s also changed in other worktrees", plural(len(shared), "file"))))
		out = append(out, "  "+st.faint.Render("These are where merge conflicts will come from."))
		out = append(out, shared...)
	}

	acts := [][2]string{{":", "run git"}, {"e", "open in " + m.editorName()}, {"f", "open folder"}, {"y", "copy path"}}
	if !s.Main && !s.Bare {
		acts = append(acts, [2]string{"d", "remove"})
	}
	out = append(out, "", "  "+m.hints(acts))

	if len(m.snap.States) == 1 {
		out = append(out, "",
			"  "+st.bold.Render("Work on a second feature side by side"),
			"  "+st.muted.Render("Each worktree is its own folder and branch, so nothing needs stashing."),
			"",
			"    "+m.hints([][2]string{{"n", "create a worktree"}}),
		)
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
			lines = append(lines, st.key.Render(fmt.Sprintf("%-7s", k[0]))+st.keyDesc.Render(k[1]))
		}
		return lipgloss.NewStyle().Width(30).Render(strings.Join(lines, "\n"))
	}
	var cols []string
	for _, sec := range helpSections {
		cols = append(cols, col(sec))
	}
	oneRow := lipgloss.JoinHorizontal(lipgloss.Top, cols...)
	twoRows := lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.JoinHorizontal(lipgloss.Top, cols[0], cols[1]), "",
		lipgloss.JoinHorizontal(lipgloss.Top, cols[2], cols[3]))

	legend := strings.Join([]string{
		st.warn.Render("●") + st.muted.Render(" uncommitted edits   ") +
			st.warn.Bold(true).Render("⚠") + st.muted.Render(" also changed in another worktree"),
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
		{oneRow, true, 1}, {twoRows, true, 1}, {twoRows, true, 0}, {twoRows, false, 0},
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
