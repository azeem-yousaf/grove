package ui

import (
	"fmt"
	"image/color"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/charmbracelet/x/ansi"
)

// overviewStats sums up every worktree and its agent.
type overviewStats struct {
	worktrees, active, clean, broken int
	files, added, deleted            int
	ahead, behind                    int
	uncommitted                      int // worktrees with work not committed yet
	conflicted                       int // worktrees with unresolved merge conflicts
	overlaps                         int // files changed in more than one worktree, not predicted to conflict
	conflicts                        int // files predicted to conflict when merged
	working, waiting, failed, done   int // agents
}

func (m *Model) overviewStats() overviewStats {
	var o overviewStats
	o.worktrees = len(m.snap.States)
	o.overlaps = m.cleanOverlapCount()
	o.conflicts = m.conflictCount()
	for _, s := range m.snap.States {
		switch {
		case s.Err != nil:
			o.broken++
		case len(s.Files) == 0:
			o.clean++
		default:
			o.active++
		}
		o.files += len(s.Files)
		o.added += s.Added
		o.deleted += s.Deleted
		o.ahead += s.Ahead
		o.behind += s.Behind
		if hasUncommitted(s) {
			o.uncommitted++
		}
		if conflictedFiles(s) > 0 {
			o.conflicted++
		}
	}
	// Only agents whose worktree still exists.
	for _, s := range m.snap.States {
		ses := m.sessionFor(s.Path)
		switch {
		case ses == nil:
		case ses.running:
			o.working++
		case len(ses.pending) > 0:
			o.waiting++
		case ses.failed:
			o.failed++
		case !ses.seen:
			o.done++
		}
	}
	return o
}

func conflictedFiles(s git.State) int {
	n := 0
	for _, f := range s.Files {
		if f.Status == git.Conflicted {
			n++
		}
	}
	return n
}

// dashHeight is the height of the summary above the worktree list, or 0
// when the pane is too short to spare it.
func (m *Model) dashHeight() int {
	_, _, paneH := m.layout()
	if m.snap == nil || paneH < 14 {
		return 0
	}
	return 1
}

// dashboardPane is the one-line summary of every worktree above the list:
// what needs the user first, then what is going on, then the counts, as
// many as fit. The header already shows how many worktrees there are and
// the overlap and conflict counts, so they aren't repeated here.
func (m *Model) dashboardPane(w, _ int) string {
	st := m.st
	o := m.overviewStats()

	var items []string
	if o.conflicted > 0 {
		items = append(items, st.del.Bold(true).Render(fmt.Sprintf("✕ %s in a merge", plural(o.conflicted, "worktree"))))
	}
	if o.waiting > 0 {
		items = append(items, st.warn.Bold(true).Render(fmt.Sprintf("✦ %s waiting", plural(o.waiting, "agent"))))
	}
	if o.failed > 0 {
		items = append(items, st.del.Render(fmt.Sprintf("✦ %s failed", plural(o.failed, "agent"))))
	}
	if len(items) == 0 && o.conflicts == 0 && o.overlaps == 0 {
		items = append(items, st.add.Render("✓ nothing needs you"))
	}
	if p := m.activePlan(); p != nil {
		c := planCounts(p)
		items = append(items, st.accent.Render(fmt.Sprintf("▸ %s %d/%d", ansi.Truncate(p.Title, 18, "…"), c[plan.Finished], len(p.Tasks))))
	}
	if o.working > 0 {
		items = append(items, st.accent.Render(fmt.Sprintf("✦ %d working", o.working)))
	}
	if o.done > 0 {
		items = append(items, st.add.Render(fmt.Sprintf("✦ %d done", o.done)))
	}
	if o.active > 0 {
		items = append(items, st.accent.Render(fmt.Sprintf("● %d active", o.active)))
	}
	if o.clean > 0 {
		items = append(items, st.muted.Render(fmt.Sprintf("○ %d clean", o.clean)))
	}
	if o.broken > 0 {
		items = append(items, st.del.Render(fmt.Sprintf("✕ %d unreadable", o.broken)))
	}
	if o.ahead > 0 {
		items = append(items, st.accent.Render(fmt.Sprintf("↑%d ahead", o.ahead)))
	}
	if o.behind > 0 {
		items = append(items, st.warn.Render(fmt.Sprintf("↓%d behind", o.behind)))
	}

	more := st.faint.Render("w overview ")
	room := w - ansi.StringWidth(more) - 2
	sep := st.faint.Render(" · ")
	line := ""
	for _, it := range items {
		next := it
		if line != "" {
			next = line + sep + it
		}
		if ansi.StringWidth(next) > room {
			break
		}
		line = next
	}
	return spread(" "+line, more, w)
}

// attention is one thing in the overview that needs the user.
type attention struct {
	icon, text string
	path       string // worktree it is about
}

// attentionItems lists what needs the user, most urgent first.
func (m *Model) attentionItems() []attention {
	st := m.st
	var urgent, later []attention
	for _, s := range m.snap.States {
		ses := m.sessionFor(s.Path)
		switch {
		case s.Err != nil:
			urgent = append(urgent, attention{st.del.Render("✕"), s.Name() + " couldn't be read: " + s.Err.Error(), s.Path})
		case s.Prunable:
			later = append(later, attention{st.faint.Render("○"), s.Name() + "'s folder is gone; git worktree prune tidies it up", s.Path})
		}
		if n := conflictedFiles(s); n > 0 {
			urgent = append(urgent, attention{st.del.Bold(true).Render("✕"), fmt.Sprintf("%s has %s; press c to resolve", s.Name(), plural(n, "unresolved conflict")), s.Path})
		}
		if ses != nil {
			switch {
			case len(ses.pending) > 0:
				urgent = append(urgent, attention{st.warn.Bold(true).Render("✦"), s.Name() + "'s agent is waiting for you: " + firstLine(ses.pending[0].text), s.Path})
			case ses.failed:
				urgent = append(urgent, attention{st.del.Render("✦"), s.Name() + "'s agent stopped with an error", s.Path})
			case !ses.running && !ses.seen:
				later = append(later, attention{st.add.Render("✦"), s.Name() + "'s agent finished; review what it changed", s.Path})
			}
		}
		if guarded, known, why := m.guardStatus(s.Path); known && !guarded && slices.ContainsFunc(m.agentDirs(), func(d string) bool { return git.SamePath(d, s.Path) }) {
			later = append(later, attention{st.warn.Render("🛡"), s.Name() + " is unguarded: git writes need you (" + why + ")", s.Path})
		}
		if s.Behind > 0 && s.Base != "" && s.Branch != s.Base {
			later = append(later, attention{st.warn.Render("↓"), fmt.Sprintf("%s is %s behind %s", s.Name(), plural(s.Behind, "commit"), s.Base), s.Path})
		}
	}
	var clashes []attention
	for path, idx := range m.snap.Overlaps {
		var names, clash []string
		first := -1
		for _, i := range idx {
			names = append(names, m.snap.States[i].Name())
			if m.willConflict(i, path) {
				clash = append(clash, m.snap.States[i].Name())
				if first < 0 {
					first = i
				}
			}
		}
		if len(clash) > 0 {
			clashes = append(clashes, attention{st.conflict.Render("✗"), path + " will conflict between " + strings.Join(clash, ", ") + "; merge one and update the other", m.snap.States[first].Path})
			continue
		}
		later = append(later, attention{st.warn.Render("⚠"), path + " is changed in " + strings.Join(names, ", "), m.snap.States[idx[0]].Path})
	}
	sortAttention(clashes)
	urgent = append(urgent, clashes...)
	for _, p := range m.plans {
		if p.Status == plan.Draft {
			continue
		}
		c := planCounts(p)
		for _, t := range p.Tasks {
			if t.Status == plan.Failed {
				urgent = append(urgent, attention{st.del.Render("✕"), fmt.Sprintf("%s (%s) failed: %s", t.Title, p.Title, t.Error), t.Path})
			}
			if c := m.checkIn(t.Path); c != nil {
				later = append(later, attention{st.accent.Render("●"), fmt.Sprintf("%s: grove is running %s for the planner", t.Title, c.command), t.Path})
			}
		}
		for _, g := range p.Groups {
			if log, ok := m.groupLog(p, g.ID); ok {
				if gs := m.groups[log.Dir]; gs != nil && gs.paused {
					path := ""
					if mem := p.Members(g.ID); len(mem) > 0 {
						path = mem[0].Path
					}
					urgent = append(urgent, attention{st.warn.Bold(true).Render("⇄"), fmt.Sprintf("%s's agents are waiting for you to post in their group chat (t)", g.Name), path})
				}
			}
		}
		switch {
		case p.Status == plan.Review && p.Merge != nil && m.testingPlan[p.ID]:
			later = append(later, attention{st.accent.Render("●"), fmt.Sprintf("%s is merged into %s; grove is running its tests", p.Title, p.Merge.Branch), p.Merge.Path})
		case p.Status == plan.Review && p.Merge != nil && p.Merge.Tests == plan.TestsFailed:
			later = append(later, attention{st.warn.Render("●"), fmt.Sprintf("%s: the merged tests failed; the planner is getting them fixed", p.Title), p.Merge.Path})
		case p.Status == plan.Review && p.Merge != nil:
			urgent = append(urgent, attention{st.accent.Bold(true).Render("✦"), fmt.Sprintf("%s is merged into %s and waits for your feedback in its plan chat (t)", p.Title, p.Merge.Branch), p.Merge.Path})
		case p.Status == plan.Done && p.Merge != nil:
			later = append(later, attention{st.add.Render("✓"), fmt.Sprintf("%s is done; ship %s from its worktree (s)", p.Title, p.Merge.Branch), p.Merge.Path})
		case p.Status == plan.Done:
			later = append(later, attention{st.add.Render("✓"), p.Title + " is done; review its worktrees and merge them with c", ""})
		case c[plan.Queued] > 0:
			later = append(later, attention{st.faint.Render("◌"), fmt.Sprintf("%s: %s waiting for a free agent slot", p.Title, plural(c[plan.Queued], "task")), ""})
		}
	}
	// Map order is random; keep the list steady between refreshes.
	sortAttention(later)
	return append(urgent, later...)
}

// sortAttention orders a by icon, then text. There can be thousands (one
// per overlapping file) and it runs every frame, so each key is worked out
// once.
func sortAttention(a []attention) {
	type keyed struct {
		key string
		a   attention
	}
	k := make([]keyed, len(a))
	for i, x := range a {
		k[i] = keyed{ansi.Strip(x.icon) + x.text, x}
	}
	slices.SortStableFunc(k, func(x, y keyed) int { return strings.Compare(x.key, y.key) })
	for i := range k {
		a[i] = k[i].a
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// openOverview shows the overview of every worktree, starting on the
// selected one.
func (m *Model) openOverview() tea.Cmd {
	if m.snap == nil {
		return nil
	}
	m.overview = true
	m.ovPath = ""
	if r, ok := m.selected(); ok {
		m.ovPath = m.snap.States[r.wt].Path
	}
	return nil
}

// ovIndex is the worktree under the overview's cursor.
func (m *Model) ovIndex() int {
	if i := m.snapIndex(m.ovPath); i >= 0 {
		return i
	}
	return 0
}

func (m *Model) ovMove(delta int) {
	if m.snap == nil || len(m.snap.States) == 0 {
		return
	}
	i := max(0, min(len(m.snap.States)-1, m.ovIndex()+delta))
	m.ovPath = m.snap.States[i].Path
}

// ovSelect closes the overview with its worktree selected in the list.
func (m *Model) ovSelect() {
	m.overview = false
	if m.snap == nil || len(m.snap.States) == 0 {
		return
	}
	path := m.snap.States[m.ovIndex()].Path
	m.unfold(path)
	m.lastSel = path
	m.focus = focusList
	m.rebuildRows()
}

func (m *Model) onOverviewKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c":
		return m.quitNow()
	case "q":
		return m.quit()
	case "esc", "w":
		m.overview = false
	case "up", "k":
		m.ovMove(-1)
	case "down", "j":
		m.ovMove(1)
	case "home", "g":
		m.ovMove(-len(m.snap.States))
	case "end", "G":
		m.ovMove(len(m.snap.States))
	case "enter":
		m.ovSelect()
		return m.selectionChanged(false)
	case "a":
		m.ovSelect()
		return tea.Batch(m.selectionChanged(false), m.openChat())
	case "c":
		m.ovSelect()
		return tea.Batch(m.selectionChanged(false), m.openConflictReview())
	case "r":
		m.toastID++
		return tea.Batch(m.startScan(), m.notify("Refreshing…", toastInfo))
	case "?":
		m.showHelp = true
	}
	return nil
}

func (m *Model) onOverviewClick(msg tea.MouseClickMsg) tea.Cmd {
	i := msg.Y - m.ovRowsY + m.ovStart
	if msg.Button != tea.MouseLeft || msg.Y < m.ovRowsY || i < m.ovStart || i >= m.ovEnd {
		return nil
	}
	path := m.snap.States[i].Path
	double := path == m.ovPath && time.Since(m.lastClickAt) < 400*time.Millisecond
	m.ovPath, m.lastClickAt = path, time.Now()
	if double {
		m.ovSelect()
		return m.selectionChanged(false)
	}
	return nil
}

// ovColumn is one column of the overview table.
type ovColumn struct {
	title string
	width int
	align byte // 'r' for right, as padCell takes
	cell  func(i int, s git.State) string
}

// overviewPane draws every worktree as a table, with what needs attention
// under it. It replaces both panes.
func (m *Model) overviewPane(w, h int) string {
	st := m.st
	inner := w - 2
	ih := h - 2
	o := m.overviewStats()
	sep := st.faint.Render("  ·  ")

	totals := []string{
		st.bold.Render(plural(o.worktrees, "worktree")),
		st.accent.Render(fmt.Sprintf("● %d active", o.active)) + "  " + st.add.Render(fmt.Sprintf("○ %d clean", o.clean)),
		st.text.Render(plural(o.files, "changed file")) + "  " + st.add.Render("+"+compact(o.added)) + " " + st.del.Render("−"+compact(o.deleted)),
	}
	if o.ahead+o.behind > 0 {
		totals = append(totals, st.accent.Render(fmt.Sprintf("↑%d", o.ahead))+" "+st.warn.Render(fmt.Sprintf("↓%d", o.behind)))
	}
	agents := []string{}
	for _, a := range []struct {
		n    int
		text string
		sty  lipgloss.Style
	}{{o.working, "working", st.accent}, {o.waiting, "waiting for you", st.warn}, {o.done, "done", st.add}, {o.failed, "failed", st.del}} {
		if a.n > 0 {
			agents = append(agents, a.sty.Render(fmt.Sprintf("%d %s", a.n, a.text)))
		}
	}
	if len(agents) > 0 {
		totals = append(totals, st.muted.Render("agents ")+strings.Join(agents, st.faint.Render(", ")))
	}
	if o.uncommitted > 0 {
		totals = append(totals, st.warn.Render(fmt.Sprintf("● %d with uncommitted work", o.uncommitted)))
	}
	if u := m.totalUsage(); u != "" {
		totals = append(totals, u+st.faint.Render(" (T)"))
	}

	cols := m.overviewColumns(inner)
	header := "  "
	for _, c := range cols {
		header += fitCell(st.faint.Render(c.title), c.width, c.align) + " "
	}

	items := m.attentionItems()
	n := len(m.snap.States)
	plans := m.planLines(inner, 3)
	// Short screens keep the table and leave the pairs to the attention list.
	clashes := m.conflictSummary(inner, min(4, max(ih-n-8, 0)))
	above := 3 // the totals, a gap and the column titles
	if len(plans) > 0 {
		above += len(plans) + 1
	}
	if len(clashes) > 0 {
		above += len(clashes) + 1
	}
	avail := max(ih-above, 1)
	tableRoom, attnRoom := min(n, avail), 0
	if len(items) > 0 {
		// Attention always keeps a few lines; the table gets the rest.
		tableRoom = min(n, max(avail-attnRoomLines(min(len(items), 3)), 1))
		attnRoom = min(len(items), max(avail-tableRoom-2, 0))
	}

	body := []string{" " + strings.Join(totals, sep), ""}
	if len(plans) > 0 {
		body = append(append(body, plans...), "")
	}
	if len(clashes) > 0 {
		body = append(append(body, clashes...), "")
	}
	body = append(body, header)
	cur := m.ovIndex()
	m.ovStart = max(0, min(cur-tableRoom/2, n-tableRoom))
	m.ovEnd = min(n, m.ovStart+tableRoom)
	m.ovRowsY = 1 + 1 + len(body) // header line, the pane's top border, the lines above
	for i := m.ovStart; i < m.ovEnd; i++ {
		s := m.snap.States[i]
		selected := i == cur
		line := "  "
		if selected {
			line = st.accent.Render("› ")
		}
		for _, c := range cols {
			line += fitCell(c.cell(i, s), c.width, c.align) + " "
		}
		var bg color.Color
		if selected {
			bg = st.p.selBg
		}
		body = append(body, fill(line, inner, bg))
	}
	if attnRoom > 0 {
		body = append(body, "", " "+st.bold.Render("Needs attention"))
		for i, a := range items[:attnRoom] {
			if i == attnRoom-1 && len(items) > attnRoom {
				body = append(body, "   "+st.faint.Render(fmt.Sprintf("… and %d more", len(items)-i)))
				break
			}
			body = append(body, "   "+a.icon+" "+st.text.Render(ansi.Truncate(a.text, inner-6, "…")))
		}
	} else if len(items) == 0 && ih-len(body) >= 3 {
		body = append(body, "", " "+st.add.Render("✓ Nothing needs you right now"))
	}

	foot := ""
	if n > m.ovEnd-m.ovStart {
		foot = fmt.Sprintf("%d–%d of %d", m.ovStart+1, m.ovEnd, n)
	}
	info := st.muted.Render("vs " + m.baseLabel())
	return m.panel("Overview of every worktree", info, foot, body, w, h, true)
}

func attnRoomLines(n int) int {
	if n == 0 {
		return 0
	}
	return n + 2 // a blank line and the title
}

// overviewColumns picks the columns that fit width, dropping the least
// useful first, and gives the spare room to the text columns.
func (m *Model) overviewColumns(width int) []ovColumn {
	st := m.st
	nameW := 12
	for i, s := range m.snap.States {
		w := ansi.StringWidth(s.Name()) + 3 // the dot, and a gap after
		if i == m.hereIdx {
			w += 2
		}
		nameW = max(nameW, min(w, 34))
	}
	name := ovColumn{"WORKTREE", nameW, 'l', func(i int, s git.State) string {
		dot := st.add.Render("○")
		switch {
		case s.Err != nil:
			dot = st.del.Render("✕")
		case conflictedFiles(s) > 0:
			dot = st.del.Bold(true).Render("●")
		case len(s.Files) > 0:
			dot = st.accent.Render("●")
		}
		label := st.text.Render(s.Name())
		if i == m.hereIdx {
			label += st.accent.Render(" ◆")
		}
		return dot + " " + label
	}}
	files := ovColumn{"FILES", 5, 'r', func(_ int, s git.State) string {
		if len(s.Files) == 0 {
			return st.faint.Render("–")
		}
		return st.text.Render(fmt.Sprint(len(s.Files)))
	}}
	changes := ovColumn{"CHANGES", 13, 'r', func(_ int, s git.State) string {
		if s.Added+s.Deleted == 0 {
			return st.faint.Render("–")
		}
		return st.add.Render("+"+compact(s.Added)) + " " + st.del.Render("−"+compact(s.Deleted))
	}}
	commits := ovColumn{"↑↓ BASE", 9, 'r', func(_ int, s git.State) string {
		if s.Base == "" || s.Branch == s.Base {
			return st.faint.Render("–")
		}
		return st.accent.Render(fmt.Sprintf("↑%d", s.Ahead)) + " " + st.warn.Render(fmt.Sprintf("↓%d", s.Behind))
	}}
	flags := ovColumn{"NOTES", 11, 'l', func(i int, s git.State) string {
		var f []string
		if n := conflictedFiles(s); n > 0 {
			f = append(f, st.del.Bold(true).Render(fmt.Sprintf("✕%d", n)))
		}
		n := 0
		for _, fc := range s.Files {
			if fc.Uncommitted {
				n++
			}
		}
		if n > 0 {
			f = append(f, st.warn.Render(fmt.Sprintf("●%d", n)))
		}
		o, c := 0, 0
		for _, fc := range s.Files {
			switch {
			case m.willConflict(i, fc.Path):
				c++
			case len(m.snap.Overlaps[fc.Path]) > 1:
				o++
			}
		}
		if c > 0 {
			f = append(f, st.conflict.Render(fmt.Sprintf("✗%d", c)))
		}
		if o > 0 {
			f = append(f, st.warn.Bold(true).Render(fmt.Sprintf("⚠%d", o)))
		}
		return strings.Join(f, " ")
	}}
	agentCol := ovColumn{"AGENT", 16, 'l', func(_ int, s git.State) string {
		if b := m.agentBadge(s.Path); b != "" {
			if guarded, known, _ := m.guardStatus(s.Path); known {
				b = map[bool]string{true: "🛡 ", false: st.warn.Render("! ")}[guarded] + b
			}
			return b
		}
		return st.faint.Render("–")
	}}
	last := ovColumn{"LAST COMMIT", 0, 'l', func(_ int, s git.State) string {
		if s.Last == nil {
			return ""
		}
		return st.faint.Render(ago(m.now, s.Last.When)+" ") + st.muted.Render(s.Last.Subject)
	}}
	task := ovColumn{"TASK", 0, 'l', func(_ int, s git.State) string {
		if p, t := m.taskFor(s.Path); t != nil {
			out := m.taskIcon(t) + " " + st.text.Render(t.Title)
			if m.checkIn(t.Path) != nil {
				out += st.warn.Render(" testing…")
			}
			if g, ok := p.Group(t.Group); ok {
				out += st.warn.Render(" ⇄ " + g.Name)
			}
			return out
		}
		if ses := m.sessionFor(s.Path); ses != nil && ses.lastUser != "" {
			return st.muted.Render(firstLine(ses.lastUser))
		}
		return ""
	}}

	// Columns in order of importance; the table keeps as many as fit.
	order := []ovColumn{name, files, changes, agentCol, commits, flags}
	used := 2 // the cursor mark
	var keep []ovColumn
	for _, c := range order {
		if used+c.width+1 > width {
			break
		}
		keep = append(keep, c)
		used += c.width + 1
	}
	room := width - used
	switch {
	case room >= 60:
		last.width = room/2 - 1
		task.width = room - last.width - 2
		keep = append(keep, last, task)
	case room >= 18:
		last.width = room - 1
		keep = append(keep, last)
	}
	return keep
}

// fitCell truncates or pads a styled string to exactly width.
func fitCell(s string, width int, align byte) string {
	if ansi.StringWidth(s) > width {
		return ansi.Truncate(s, width, "…")
	}
	return padCell(s, width, align)
}

// planCounts counts a plan's tasks by status.
func planCounts(p *plan.Plan) map[plan.TaskStatus]int {
	c := map[plan.TaskStatus]int{}
	for _, t := range p.Tasks {
		c[t.Status]++
	}
	return c
}

// activePlan is the newest running plan, if any.
func (m *Model) activePlan() *plan.Plan {
	for _, p := range m.plans {
		if p.Status == plan.Running {
			return p
		}
	}
	return nil
}

// taskIcon shows a task's status.
func (m *Model) taskIcon(t *plan.Task) string {
	st := m.st
	if m.checkIn(t.Path) != nil {
		// grove is testing it for the planner.
		return st.warn.Render(spinnerFrames[m.frame%len(spinnerFrames)])
	}
	switch t.Status {
	case plan.Working:
		if s := m.sessionFor(t.Path); s != nil && s.running {
			return st.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)])
		}
		return st.accent.Render("●")
	case plan.Finished:
		return st.add.Render("✓")
	case plan.Failed:
		return st.del.Render("✕")
	}
	return st.faint.Render("◌")
}

// conflictSummary is the overview's Conflicts section: a title and which
// worktrees conflict with which, in how many files, at most most lines in
// all. It is empty when nothing conflicts or there is no room.
func (m *Model) conflictSummary(width, most int) []string {
	st := m.st
	pairs := m.conflictPairs()
	if len(pairs) == 0 || most < 2 {
		return nil
	}
	out := []string{" " + st.conflict.Render("✗ Conflicts") + st.faint.Render(" · predicted by test-merging in git's object store")}
	for i, p := range pairs {
		if len(out) == most-1 && len(pairs)-i > 1 {
			out = append(out, "   "+st.faint.Render(fmt.Sprintf("… and %d more pairs", len(pairs)-i)))
			break
		}
		line := "   " + st.text.Render(m.snap.States[p.A].Name()) + st.faint.Render(" ⇄ ") + st.text.Render(m.snap.States[p.B].Name()) +
			"  " + st.conflict.Render(plural(len(p.Files), "file")) + st.faint.Render(": "+p.Files[0].Path)
		if len(p.Files) > 1 {
			line += st.faint.Render(fmt.Sprintf(" +%d", len(p.Files)-1))
		}
		out = append(out, ansi.Truncate(line, width, "…"))
	}
	return out
}

// planLines summarise the newest started plans for the overview.
func (m *Model) planLines(width, most int) []string {
	st := m.st
	var out []string
	for _, p := range m.plans {
		if p.Status == plan.Draft {
			continue
		}
		if len(out) == most {
			break
		}
		c := planCounts(p)
		status := st.accent.Render("running")
		switch p.Status {
		case plan.Review:
			status = st.warn.Render("merged, in review")
		case plan.Done:
			status = st.add.Render("done")
		}
		parts := []string{st.add.Render(fmt.Sprintf("✓ %d finished", c[plan.Finished]))}
		if n := c[plan.Working]; n > 0 {
			parts = append(parts, st.accent.Render(fmt.Sprintf("● %d working", n)))
		}
		if n := c[plan.Queued]; n > 0 {
			parts = append(parts, st.faint.Render(fmt.Sprintf("◌ %d waiting", n)))
		}
		if n := c[plan.Failed]; n > 0 {
			parts = append(parts, st.del.Render(fmt.Sprintf("✕ %d failed", n)))
		}
		groups := ""
		if len(p.Groups) > 0 {
			groups = st.faint.Render(" · ") + st.warn.Render(plural(len(p.Groups), "group"))
		}
		tokens := ""
		if u := m.planUsage(p); u != "" {
			tokens = st.faint.Render(" · ") + u
		}
		line := " " + st.bold.Render("▸ "+p.Title) + st.faint.Render(" · ") + status + st.faint.Render(fmt.Sprintf(" · %d tasks: ", len(p.Tasks))) +
			strings.Join(parts, st.faint.Render(", ")) + groups + tokens
		out = append(out, ansi.Truncate(line, width, "…"))
	}
	return out
}
