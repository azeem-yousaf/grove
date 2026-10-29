package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/charmbracelet/x/ansi"
)

// Reviewing a worktree's work in the diff pane:
//
//   - v shows every file's diff in one scroll; v again shows only what
//     changed since its agent's last turn began (from session.turnTree).
//   - / searches the diff shown, n and N then go from match to match.
//   - x puts back the hunk at the top of the pane, after asking.
//   - > opens the agent's chat with that hunk quoted, for a note about it.

// wtView is the whole-worktree diff, while shown.
type wtView struct {
	path, name string
	since      bool   // since the agent's last turn, rather than all changes
	loading    string // key of the diff being loaded
	loaded     string // key of raw
	raw        string
	err        error
}

type wtDiffMsg struct {
	key, raw string
	err      error
}

// toggleWorktreeView opens the whole-worktree diff for the selected
// worktree, then switches it to the last turn and back.
func (m *Model) toggleWorktreeView() tea.Cmd {
	if v := m.wtView; v != nil {
		if !v.since {
			if s := m.sessionFor(v.path); s == nil || s.turnTree == "" {
				return m.notify("Its agent hasn't had a turn yet, so there's only everything to show", toastInfo)
			}
		}
		v.since = !v.since
		m.curHunk = -1
		m.diff.GotoTop()
		return m.loadWorktreeView()
	}
	r, ok := m.selected()
	if !ok {
		return nil
	}
	s := m.snap.States[r.wt]
	if s.Bare || s.Prunable || s.Err != nil {
		return m.notify("That worktree has no files to show", toastInfo)
	}
	m.wtView = &wtView{path: s.Path, name: s.Name()}
	m.curHunk = -1
	m.focus = focusDiff
	m.diff.GotoTop()
	m.diff.SetXOffset(0)
	return m.loadWorktreeView()
}

// wtState is the worktree the view is for, from the latest scan.
func (m *Model) wtState() (git.State, bool) {
	if m.wtView == nil || m.snap == nil {
		return git.State{}, false
	}
	for _, s := range m.snap.States {
		if git.SamePath(s.Path, m.wtView.path) {
			return s, true
		}
	}
	return git.State{}, false
}

// wtKey names what the view should show: it changes when the scan finds
// the worktree's files changed.
func (m *Model) wtKey(s git.State) string {
	var b strings.Builder
	if m.wtView.since {
		tree := ""
		if ses := m.sessionFor(s.Path); ses != nil {
			tree = ses.turnTree
		}
		fmt.Fprintf(&b, "since|%s|%s", s.Path, tree)
	} else {
		fmt.Fprintf(&b, "all|%s|%s", s.Path, s.Rev)
	}
	for _, f := range s.Files {
		fmt.Fprintf(&b, "|%s:%c:%d:%d:%t", f.Path, f.Status, f.Added, f.Deleted, f.Uncommitted)
	}
	return b.String()
}

// loadWorktreeView loads the view's diff unless it is loaded or loading.
func (m *Model) loadWorktreeView() tea.Cmd {
	v := m.wtView
	s, ok := m.wtState()
	if v == nil || !ok {
		return nil
	}
	key := m.wtKey(s)
	if key == v.loaded || key == v.loading {
		return nil
	}
	v.loading = key
	since := ""
	if v.since {
		if ses := m.sessionFor(s.Path); ses != nil {
			since = ses.turnTree
		}
	}
	ctx := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var raw string
		var err error
		if since != "" {
			raw, err = git.DiffSince(ctx, s.Path, since)
		} else {
			raw, err = git.WorktreeDiff(ctx, s)
		}
		return wtDiffMsg{key, raw, err}
	}
}

func (m *Model) onWorktreeDiff(msg wtDiffMsg) tea.Cmd {
	v := m.wtView
	if v == nil || msg.key != v.loading {
		return nil
	}
	v.loading, v.loaded, v.raw, v.err = "", msg.key, msg.raw, msg.err
	return nil
}

// refreshWorktreeView renders the view into the diff viewport.
func (m *Model) refreshWorktreeView(inner, paneH int) {
	v := m.wtView
	m.diff.SetWidth(inner)
	m.diff.SetHeight(max(paneH-2, 1))
	want := fmt.Sprintf("wt|%s|%d|%t|%s", v.loaded, inner, m.dark, m.searchQuery)
	if v.loaded == "" {
		want = "wt|loading"
	}
	if want == m.renderedFor {
		return
	}
	m.renderedFor = want
	m.shownKey = "wt"
	m.diff.LeftGutterFunc = nil
	pd := parseDiffs(v.raw, true, maxWorktreeLines)
	switch {
	case v.loaded == "":
		m.parsed = parsedDiff{}
		m.diff.SetContentLines(m.placeholder("", "Loading every change…", inner))
	case v.err != nil:
		m.parsed = parsedDiff{}
		m.diff.SetContentLines(m.placeholder("✕", "Couldn't load the diff\n"+v.err.Error(), inner))
	case len(pd.lines) == 0:
		m.parsed = parsedDiff{}
		msg := "No changes"
		if v.since {
			msg = "Nothing changed since the agent's last turn began"
		}
		m.diff.SetContentLines(m.placeholder("○", msg, inner))
	default:
		m.parsed = pd
		content, gutter := renderDiff(pd, "", inner, m.st, nil, m.searchQuery)
		m.diff.LeftGutterFunc = m.diffGutter(gutter)
		m.diff.SetContentLines(content)
	}
}

// wtTitle is the panel's info for the view: what it shows.
func (m *Model) wtTitle() string {
	v := m.wtView
	what := "all changes"
	if v.since {
		what = "since the agent's last turn"
	}
	files := 0
	for _, l := range m.parsed.lines {
		if l.kind == lineFile {
			files++
		}
	}
	info := m.st.accent.Render(what)
	if files > 0 {
		info += m.st.faint.Render(" · " + plural(files, "file"))
	}
	return info
}

// jumpFile scrolls the view to the next or previous file.
func (m *Model) jumpFile(dir int) {
	y := m.diff.YOffset()
	lines := m.parsed.lines
	for i := y + dir; i >= 0 && i < len(lines); i += dir {
		if lines[i].kind == lineFile {
			m.diff.SetYOffset(i)
			if i+1 < len(lines) && lines[i+1].hunk >= 0 {
				m.curHunk = lines[i+1].hunk
			}
			return
		}
	}
	if dir < 0 {
		m.diff.GotoTop()
	}
}

// Search.

func newSearchInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "/ "
	ti.Placeholder = "search the diff"
	return ti
}

func (m *Model) startSearch() tea.Cmd {
	if len(m.parsed.lines) == 0 {
		return m.notify("Nothing to search here", toastInfo)
	}
	m.searchActive = true
	m.searchFrom = m.diff.YOffset()
	m.searchInput.SetValue(m.searchQuery)
	m.searchInput.CursorEnd()
	return m.searchInput.Focus()
}

func (m *Model) onSearchKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.clearSearch()
		return nil
	case "enter":
		m.searchActive = false
		m.searchInput.Blur()
		return nil
	case "ctrl+c":
		return m.quitNow()
	case "down", "ctrl+n":
		m.jumpMatch(1)
		return nil
	case "up", "ctrl+p":
		m.jumpMatch(-1)
		return nil
	}
	var cmd tea.Cmd
	m.searchInput, cmd = m.searchInput.Update(msg)
	m.setSearch(m.searchInput.Value())
	return cmd
}

// setSearch searches for q from where the search started.
func (m *Model) setSearch(q string) {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == m.searchQuery {
		return
	}
	m.searchQuery = q
	m.searchAt = m.searchFrom - 1
	m.jumpMatch(1)
}

func (m *Model) clearSearch() {
	m.searchActive, m.searchQuery = false, ""
	m.searchInput.Blur()
	m.searchInput.SetValue("")
}

// matches are the lines of the shown diff that contain the search.
func (m *Model) matches() []int {
	if m.searchQuery == "" {
		return nil
	}
	var out []int
	for i, l := range m.parsed.lines {
		if strings.Contains(strings.ToLower(l.text), m.searchQuery) {
			out = append(out, i)
		}
	}
	return out
}

// jumpMatch scrolls to the next or previous match, wrapping around, with
// a few lines above it for context.
func (m *Model) jumpMatch(dir int) {
	hits := m.matches()
	if len(hits) == 0 {
		return
	}
	next := -1
	if dir > 0 {
		next = hits[0]
		for _, i := range hits {
			if i > m.searchAt {
				next = i
				break
			}
		}
	} else {
		next = hits[len(hits)-1]
		for j := len(hits) - 1; j >= 0; j-- {
			if hits[j] < m.searchAt {
				next = hits[j]
				break
			}
		}
	}
	m.searchAt = next
	m.diff.SetYOffset(max(next-3, 0))
	if h := m.parsed.lines[next].hunk; h >= 0 {
		m.curHunk = h
	}
}

// searchStatus is "3 of 12" for the footer, or "no match".
func (m *Model) searchStatus() string {
	hits := m.matches()
	if len(hits) == 0 {
		return "no match"
	}
	for n, i := range hits {
		if i == m.searchAt {
			return fmt.Sprintf("%d of %d", n+1, len(hits))
		}
	}
	return plural(len(hits), "match")
}

// Acting on a hunk.

// currentHunk is the hunk at the top of the diff pane, and the worktree
// folder its path is relative to.
func (m *Model) currentHunk() (git.Hunk, string, bool) {
	dir := ""
	if s, ok := m.wtState(); ok {
		dir = s.Path
	} else if s, _, ok := m.selectedFile(); ok {
		dir = s.Path
	}
	if dir == "" || m.shownKey == "" {
		return git.Hunk{}, "", false
	}
	i := m.currentHunkIndex()
	if i < 0 {
		return git.Hunk{}, "", false
	}
	h := m.parsed.hunks[i]
	return h, dir, h.Path != ""
}

// currentHunkIndex is the hunk x and > act on: the one last moved to (n,
// N, [ ], a search match) while any of it is on screen, else the first
// one from the top of the pane. -1 when there is none.
func (m *Model) currentHunkIndex() int {
	lines, n := m.parsed.lines, len(m.parsed.hunks)
	if n == 0 || len(lines) == 0 {
		return -1
	}
	y := min(m.diff.YOffset(), len(lines)-1)
	end := min(y+max(m.diff.Height(), 1), len(lines))
	if m.curHunk >= 0 && m.curHunk < n {
		for i := y; i < end; i++ {
			if lines[i].hunk == m.curHunk {
				return m.curHunk
			}
		}
	}
	if h, ok := m.parsed.hunkAt(y); ok {
		for i, x := range m.parsed.hunks {
			if x.Path == h.Path && x.NewStart == h.NewStart && x.OldStart == h.OldStart {
				return i
			}
		}
	}
	return -1
}

// setCurrentHunk makes hunk i current and scrolls to its first line (and
// its file's name, when it is the file's first hunk).
func (m *Model) setCurrentHunk(i int) {
	m.curHunk = i
	for j, l := range m.parsed.lines {
		if l.hunk != i {
			continue
		}
		if j > 0 && m.parsed.lines[j-1].kind == lineFile {
			j--
		}
		m.diff.SetYOffset(j)
		return
	}
}

// diffGutter shows gutter beside the diff, with a bar down the current
// hunk while the diff pane has the focus.
func (m *Model) diffGutter(gutter []string) func(viewport.GutterContext) string {
	blank := strings.Repeat(" ", ansi.StringWidth(gutter[0]))
	return func(c viewport.GutterContext) string {
		if c.Index >= len(gutter) || c.Soft {
			return blank
		}
		g := gutter[c.Index]
		if m.focus != focusDiff || c.Index >= len(m.parsed.lines) {
			return g
		}
		if l := m.parsed.lines[c.Index]; l.hunk >= 0 && l.kind != lineFile && l.hunk == m.currentHunkIndex() {
			return m.st.accent.Render("▌") + ansi.Cut(g, 1, ansi.StringWidth(g))
		}
		return g
	}
}

// revertAsk confirms putting a hunk back.
type revertAsk struct {
	dir, name string
	hunk      git.Hunk
}

type revertDoneMsg struct {
	path string
	err  error
}

func (m *Model) askRevert() tea.Cmd {
	h, dir, ok := m.currentHunk()
	if !ok {
		return m.notify("Scroll to a change to put back", toastInfo)
	}
	name := dir
	for _, s := range m.snap.States {
		if git.SamePath(s.Path, dir) {
			name = s.Name()
		}
	}
	m.revert = &revertAsk{dir: dir, name: name, hunk: h}
	return nil
}

func (m *Model) onRevertKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc", "n":
		m.revert = nil
	case "ctrl+c":
		return m.quitNow()
	case "enter", "y":
		r := m.revert
		m.revert = nil
		return func() tea.Msg { return revertDoneMsg{r.hunk.Path, git.RevertHunk(r.dir, r.hunk)} }
	}
	return nil
}

func (m *Model) onRevertDone(msg revertDoneMsg) tea.Cmd {
	if m.wtView != nil {
		m.wtView.loaded = "" // reload even if the counts come out the same
	}
	if msg.err != nil {
		return tea.Batch(m.notify("Couldn't put "+msg.path+" back: "+msg.err.Error(), toastErr), m.startScan())
	}
	return tea.Batch(m.notify("Put back that change in "+msg.path, toastOK), m.startScan())
}

func (m *Model) revertModal() string {
	st := m.st
	r := m.revert
	h := r.hunk
	w := min(72, m.width-10)
	what := fmt.Sprintf("lines %d–%d", h.NewStart, h.NewStart+max(h.NewCount-1, 0))
	switch {
	case h.NewFile && h.OldCount == 0:
		what = "the whole new file, which is deleted"
	case h.DeletedFile:
		what = "the deleted file, which comes back"
	case h.NewCount == 0:
		what = fmt.Sprintf("after line %d", h.NewStart)
	}
	parts := []string{
		st.bold.Foreground(st.p.del).Render("Put this change back"),
		"",
		st.bold.Render(h.Path) + st.faint.Render("  "+what+" · "+r.name),
		"",
	}
	room := max(min(m.height-14, 16), 3)
	for i, l := range h.Lines {
		if i == room {
			parts = append(parts, st.faint.Render(fmt.Sprintf("  … %d more lines", len(h.Lines)-room)))
			break
		}
		s := st.muted
		switch {
		case strings.HasPrefix(l, "+"):
			s = st.add
		case strings.HasPrefix(l, "-"):
			s = st.del
		}
		parts = append(parts, s.Render(ansi.Truncate(expandTabs(l), w, "…")))
	}
	parts = append(parts, "",
		st.muted.Render("The + lines are taken out and the − lines put back, in the file on disk."),
		"", m.hints([][2]string{{"enter", "put it back"}, {"esc", "keep it"}}))
	return st.modal.BorderForeground(st.p.del).Render(strings.Join(parts, "\n"))
}

// hunkToAgent opens the worktree's agent chat with the hunk quoted, for
// the user to say what should change about it.
func (m *Model) hunkToAgent() tea.Cmd {
	h, _, ok := m.currentHunk()
	if !ok {
		return m.notify("Scroll to a change to ask the agent about", toastInfo)
	}
	lines := h.Lines
	const most = 40
	more := ""
	if len(lines) > most {
		more = fmt.Sprintf("\n… %d more lines", len(lines)-most)
		lines = lines[:most]
	}
	m.chatDraft = fmt.Sprintf("About this change to %s, from line %d:\n```diff\n%s%s\n```\n", h.Path, h.NewStart, strings.Join(lines, "\n"), more)
	m.wtView = nil
	m.clearSearch()
	cmd := m.openChat()
	if m.focus != focusChat && m.picker == nil {
		m.chatDraft = "" // no chat opened after all
	}
	return cmd
}
