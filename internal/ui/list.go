package ui

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/charmbracelet/x/ansi"
)

type rowKind uint8

const (
	rowTree rowKind = iota // a worktree header
	rowFile                // a changed file
)

// row is one selectable item in the worktree list.
type row struct {
	kind rowKind
	wt   int // index into snapshot States
	file int // index into that state's Files, for rowFile
}

// listLine is one rendered line of the list. Several lines can belong to
// one row (a worktree header takes two); spacers belong to none.
type listLine struct {
	row  int // index into rows, -1 for spacers and notes
	text string
}

// selKey identifies a row across refreshes so the selection stays put while
// the data underneath it changes.
func (m *Model) selKey(r row) string {
	st := m.snap.States[r.wt]
	if r.kind == rowTree {
		return st.Path
	}
	return st.Path + "\x00" + st.Files[r.file].Path
}

// visibleFiles applies the filter and the overlaps or conflicts view to a
// worktree.
func (m *Model) visibleFiles(wt int) []int {
	st := m.snap.States[wt]
	q := strings.ToLower(m.filterQuery)
	var out []int
	for i, f := range st.Files {
		switch {
		case m.overlapView == overlapsShown && len(m.snap.Overlaps[f.Path]) < 2:
			continue
		case m.overlapView == conflictsShown && !m.willConflict(wt, f.Path):
			continue
		}
		if q != "" && !fuzzy(strings.ToLower(f.Path), q) {
			continue
		}
		out = append(out, i)
	}
	return out
}

// filtering reports whether any filter is narrowing the list.
func (m *Model) filtering() bool { return m.filterQuery != "" || m.overlapView != overlapsOff }

// rebuildRows recomputes the selectable rows and restores the selection.
func (m *Model) rebuildRows() {
	key := m.lastSel
	m.rows = m.rows[:0]
	for i, st := range m.snap.States {
		files := m.visibleFiles(i)
		if m.filtering() && len(files) == 0 {
			continue
		}
		m.rows = append(m.rows, row{kind: rowTree, wt: i})
		if m.collapsed[st.Path] {
			continue
		}
		for _, f := range files {
			m.rows = append(m.rows, row{kind: rowFile, wt: i, file: f})
		}
	}
	m.cursor = m.findRow(key)
	m.syncSel()
}

// syncSel remembers the key of the selected row.
func (m *Model) syncSel() {
	m.lastSel = ""
	if m.cursor < len(m.rows) {
		m.lastSel = m.selKey(m.rows[m.cursor])
	}
}

// findRow locates the previously selected row by key, then its worktree,
// and otherwise keeps the cursor index in range. With no previous selection
// it starts on the worktree grove was launched from.
func (m *Model) findRow(key string) int {
	if len(m.rows) == 0 {
		return 0
	}
	if key == "" {
		for i, r := range m.rows {
			if r.wt == m.hereIdx {
				return i
			}
		}
		return 0
	}
	for i, r := range m.rows {
		if m.selKey(r) == key {
			return i
		}
	}
	wtPath, _, _ := strings.Cut(key, "\x00")
	for i, r := range m.rows {
		if r.kind == rowTree && git.SamePath(m.snap.States[r.wt].Path, wtPath) {
			// Prefer the file now at the same position under that worktree.
			if m.cursor > i && m.cursor < len(m.rows) && m.rows[m.cursor].wt == r.wt {
				return m.cursor
			}
			return i
		}
	}
	return min(m.cursor, len(m.rows)-1)
}

// listLines renders every line of the list for the given inner width.
func (m *Model) listLines(width int) []listLine {
	st := m.st
	var lines []listLine
	for ri, r := range m.rows {
		selected := ri == m.cursor
		switch r.kind {
		case rowTree:
			if len(lines) > 0 {
				lines = append(lines, listLine{row: -1})
			}
			head, meta := m.treeLines(r.wt, width, selected)
			lines = append(lines, listLine{row: ri, text: head}, listLine{row: ri, text: meta})
			if !m.collapsed[m.snap.States[r.wt].Path] {
				if note := m.treeNote(r.wt, width); note != "" {
					lines = append(lines, listLine{row: -1, text: note})
				}
			}
		case rowFile:
			lines = append(lines, listLine{row: ri, text: m.fileLine(r, width, selected)})
		}
	}
	if len(m.rows) == 0 {
		msg := "No worktrees"
		if m.filtering() {
			msg = "No files match"
			switch {
			case m.filterQuery != "":
			case m.overlapView == overlapsShown:
				msg = "No overlapping files"
			case m.overlapView == conflictsShown:
				msg = "No conflicting files"
			}
		}
		lines = append(lines, listLine{row: -1, text: ""}, listLine{row: -1, text: center(st.muted.Render(msg), width)})
	}
	return lines
}

// treeNote is the hint under an expanded worktree with nothing to list.
func (m *Model) treeNote(wt, width int) string {
	s := m.snap.States[wt]
	st := m.st
	switch {
	case s.Err != nil:
		return "     " + st.del.Render(ansi.Truncate(s.Err.Error(), width-6, "…"))
	case s.Bare:
		return "     " + st.faint.Render("bare repository, no working files")
	case len(s.Files) == 0:
		what := "nothing changed"
		if m.snap.Mode == git.ModeBranch && s.Base != "" && s.Branch != s.Base {
			what = "nothing changed since " + s.Base
		} else if m.snap.Mode == git.ModeUncommitted || s.Branch == s.Base {
			what = "no uncommitted changes"
		}
		return "     " + st.add.Render("✓") + " " + st.faint.Render(what)
	}
	return ""
}

func (m *Model) treeLines(wt, width int, selected bool) (string, string) {
	s := m.snap.States[wt]
	st := m.st
	p := st.p
	bg := m.rowBg(selected)

	chev := "▾"
	if m.collapsed[s.Path] {
		chev = "▸"
	}
	dot := st.accent.Render("●")
	switch {
	case s.Err != nil:
		dot = st.del.Render("✕")
	case len(s.Files) == 0:
		dot = st.faint.Render("○")
	}

	name := st.bold.Render(s.Name())
	if selected {
		name = st.bold.Foreground(p.accent).Render(s.Name())
	}
	var tags []string
	if wt == m.hereIdx {
		tags = append(tags, st.accent.Render("◆ here"))
	}
	if s.Base != "" && s.Branch == s.Base {
		tags = append(tags, st.faint.Render("base"))
	}
	if s.Locked {
		tags = append(tags, st.warn.Render("locked"))
	}
	// With an agent, the dot becomes its status icon; the words follow
	// below if there's room for them.
	badge := m.agentBadge(s.Path)
	if icon := m.agentIcon(s.Path); icon != "" {
		dot = icon
	}

	right := ""
	if s.Err == nil && (s.Added > 0 || s.Deleted > 0) {
		right = stat(st, s.Added, s.Deleted)
	} else if s.Err == nil && len(s.Files) > 0 {
		right = st.faint.Render("binary")
	}
	left := " " + st.faint.Render(chev) + " " + dot + " " + name
	if len(tags) > 0 {
		left += "  " + strings.Join(tags, " ")
	}
	if badge != "" && ansi.StringWidth(left)+2+ansi.StringWidth(badge)+ansi.StringWidth(right)+2 <= width {
		left += "  " + badge
	}
	head := spread(left, right+" ", width)

	var meta []string
	meta = append(meta, st.faint.Render(shortPath(s.Path, max(width/2, 16))))
	if n := len(s.Files); n > 0 {
		meta = append(meta, st.muted.Render(plural(n, "file")))
		if s.Truncated {
			meta[len(meta)-1] += st.warn.Render("+")
		}
	}
	if s.Base != "" && s.Branch != s.Base && (s.Ahead > 0 || s.Behind > 0) {
		ab := ""
		if s.Ahead > 0 {
			ab += st.accent.Render(fmt.Sprintf("↑%d", s.Ahead))
		}
		if s.Behind > 0 {
			if ab != "" {
				ab += " "
			}
			ab += st.warn.Render(fmt.Sprintf("↓%d", s.Behind))
		}
		meta = append(meta, ab)
	}
	if s.Last != nil {
		meta = append(meta, st.faint.Render(ago(m.now, s.Last.When)))
	}
	metaLine := "     " + strings.Join(meta, st.faint.Render(" · "))
	return fill(head, width, bg), fill(ansi.Truncate(metaLine, width, "…"), width, bg)
}

func (m *Model) fileLine(r row, width int, selected bool) string {
	s := m.snap.States[r.wt]
	f := s.Files[r.file]
	st := m.st
	bg := m.rowBg(selected)

	letter := statusStyle(st, f.Status).Bold(true).Render(string(f.Status))
	if f.Status == git.Untracked {
		letter = st.add.Bold(true).Render("?")
	}

	marks := ""
	// Only worktrees with an agent get the extra column, so the others keep
	// their usual layout.
	if ses := m.sessionFor(s.Path); ses != nil {
		if ses.touched[f.Path] {
			marks += st.accent.Render("✦") + " "
		} else {
			marks += "  "
		}
	}
	if f.Uncommitted && m.snap.Mode == git.ModeBranch {
		marks += st.warn.Render("●")
	} else {
		marks += " "
	}
	// ✗ will conflict when merged; ⚠ overlaps but merges cleanly, or the
	// prediction isn't in yet.
	switch {
	case m.willConflict(r.wt, f.Path):
		marks += " " + st.conflict.Render("✗")
	case len(m.snap.Overlaps[f.Path]) > 1:
		marks += " " + st.warn.Bold(true).Render("⚠")
	default:
		marks += "  "
	}

	// Markers sit in a fixed column next to the numbers so they line up
	// down the list regardless of path length.
	right := marks + " "
	if f.Binary {
		right += st.faint.Render(fmt.Sprintf("%-11s", "  binary"))
	} else {
		right += stat(st, f.Added, f.Deleted)
	}
	if width >= 56 {
		right += " " + bar(st, f.Added, f.Deleted, m.maxChange(r.wt))
	}
	right += " "

	prefix := "     " + letter + " "
	room := width - ansi.StringWidth(prefix) - ansi.StringWidth(right) - 1
	left := prefix + renderPath(st, f, max(room, 8), selected)
	return fill(spread(left, right, width), width, bg)
}

func (m *Model) rowBg(selected bool) color.Color {
	switch {
	case !selected:
		return nil
	case m.focus == focusList:
		return m.st.p.selBg
	}
	return m.st.p.selBgDim
}

// maxChange is the largest per-file change in a worktree, used to scale the
// little diffstat bars.
func (m *Model) maxChange(wt int) int {
	best := 1
	for _, f := range m.snap.States[wt].Files {
		best = max(best, f.Added+f.Deleted)
	}
	return best
}

// renderPath shows a file's folder dimmed and its name bright, trimming the
// front of the path first when space runs out.
func renderPath(st styles, f git.FileChange, room int, selected bool) string {
	path := f.Path
	if f.OldPath != "" {
		path = f.OldPath + " → " + f.Path
	}
	if utf8.RuneCountInString(path) > room {
		r := []rune(path)
		path = "…" + string(r[len(r)-room+1:])
	}
	dir, name := "", path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		dir, name = path[:i+1], path[i+1:]
	}
	nameStyle := st.text
	if selected {
		nameStyle = st.bold
	}
	if f.Status == git.Deleted {
		nameStyle = nameStyle.Strikethrough(true).Foreground(st.p.muted)
	}
	return st.muted.Render(dir) + nameStyle.Render(name)
}

func statusStyle(st styles, s git.Status) lipgloss.Style {
	switch s {
	case git.Added, git.Untracked:
		return st.add
	case git.Deleted:
		return st.del
	case git.Renamed, git.Copied:
		return st.renamed
	case git.Conflicted:
		return st.warn
	case git.TypeChanged:
		return st.muted
	}
	return st.info
}

// stat renders "+12 −3" with fixed-width columns so numbers line up.
func stat(st styles, added, deleted int) string {
	a := st.add.Render(fmt.Sprintf("%5s", "+"+compact(added)))
	d := st.del.Render(fmt.Sprintf("%-5s", "−"+compact(deleted)))
	if added == 0 {
		a = st.faint.Render(fmt.Sprintf("%5s", "+0"))
	}
	if deleted == 0 {
		d = st.faint.Render(fmt.Sprintf("%-5s", "−0"))
	}
	return a + " " + d
}

// bar is a five-cell GitHub-style diffstat: the filled length shows how big
// the change is relative to the worktree's biggest file, and the colour split
// shows additions against deletions.
func bar(st styles, added, deleted, biggest int) string {
	const cells = 5
	total := added + deleted
	if total == 0 {
		return st.faint.Render(strings.Repeat("·", cells))
	}
	filled := max(1, (total*cells+biggest-1)/biggest)
	filled = min(filled, cells)
	green := (added*filled + total/2) / total
	if added > 0 && green == 0 {
		green = 1
	}
	red := filled - green
	if deleted > 0 && red == 0 && green > 1 {
		green, red = green-1, 1
	}
	return st.add.Render(strings.Repeat("■", green)) +
		st.del.Render(strings.Repeat("■", red)) +
		st.faint.Render(strings.Repeat("·", cells-filled))
}

func compact(n int) string {
	switch {
	case n >= 100000:
		return fmt.Sprintf("%dk", n/1000)
	case n >= 10000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// shortPath abbreviates the home folder to ~ and trims the middle of long
// paths, keeping the drive and the last folder visible.
func shortPath(p string, room int) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = "~" + string(filepath.Separator) + rel
			if rel == "." {
				p = "~"
			}
		}
	}
	if utf8.RuneCountInString(p) <= room {
		return p
	}
	sep := string(filepath.Separator)
	parts := strings.Split(p, sep)
	if len(parts) >= 3 {
		short := parts[0] + sep + "…" + sep + parts[len(parts)-1]
		if utf8.RuneCountInString(short) <= room {
			return short
		}
	}
	r := []rune(p)
	return "…" + string(r[len(r)-room+1:])
}

// fuzzy reports whether every rune of q appears in s, in order.
func fuzzy(s, q string) bool {
	if strings.Contains(s, q) {
		return true
	}
	for _, r := range q {
		i := strings.IndexRune(s, r)
		if i < 0 {
			return false
		}
		s = s[i+utf8.RuneLen(r):]
	}
	return true
}
