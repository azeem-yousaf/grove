package ui

import (
	"fmt"
	"image/color"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/charmbracelet/x/ansi"
)

// The worktrees of a plan group are listed together under the group's
// header, framed by a border whose colour shows how the group is doing.
// Below groupBoxWidth the frame is only a bar down the left side, which
// costs half the columns.

const groupBoxWidth = 48

// listGroup is a plan group in the worktree list. Its plan is looked up by
// ID when drawn, since plans are replaced while the list stays put.
type listGroup struct {
	plan, id string
	members  []int // indexes into m.snap.States, in list order
}

func (g listGroup) key() string { return groupKey(g.plan, g.id) }

// groupKey identifies a group in m.collapsed and the selection, apart from
// worktree paths.
func groupKey(planID, groupID string) string { return "group\x00" + planID + "\x00" + groupID }

// findGroups gathers the worktrees of grouped tasks, for as long as their
// worktrees exist, whatever their plan's status.
func (m *Model) findGroups() []listGroup {
	var out []listGroup
	index := map[string]int{}
	for i, s := range m.snap.States {
		p, t := m.taskFor(s.Path)
		if t == nil || t.Group == "" {
			continue
		}
		if _, ok := p.Group(t.Group); !ok {
			continue
		}
		k := groupKey(p.ID, t.Group)
		gi, seen := index[k]
		if !seen {
			gi = len(out)
			index[k] = gi
			out = append(out, listGroup{plan: p.ID, id: t.Group})
		}
		out[gi].members = append(out[gi].members, i)
	}
	return out
}

// groupPlan is a listed group's plan and group. The plan is nil if it was
// deleted since the list was built.
func (m *Model) groupPlan(gi int) (*plan.Plan, plan.Group) {
	g := m.listGroups[gi]
	p := m.planByID(g.plan)
	if p != nil {
		if grp, ok := p.Group(g.id); ok {
			return p, *grp
		}
	}
	return p, plan.Group{ID: g.id, Name: g.id}
}

// groupIcon is a group's emoji, or ⇄ when it has none or one that could
// upset the layout. Only a single code point two cells wide is used, since
// terminals disagree on the width of variation selectors and joined
// sequences.
func groupIcon(g plan.Group) string {
	if usableIcon(g.Icon) {
		return g.Icon
	}
	return "⇄"
}

func usableIcon(s string) bool {
	return utf8.RuneCountInString(s) == 1 && ansi.StringWidth(s) == 2 && s != "⇄"
}

// splitGroupIcon reads "🛒 Checkout" as an icon and a name. Without a usable
// emoji first, the whole text is the name.
func splitGroupIcon(s string) (icon, name string) {
	s = strings.TrimSpace(s)
	_, size := utf8.DecodeRuneInString(s)
	first, rest := s[:size], s[size:]
	rest = strings.TrimPrefix(rest, "️") // emoji pickers add it
	if usableIcon(first) && strings.TrimSpace(rest) != "" {
		return first, strings.TrimSpace(rest)
	}
	return "", s
}

// groupStats sums up a group for its header, border and summary.
type groupStats struct {
	worktrees, files, added, deleted int
	working, waiting, failed         int // agents
	tasks, finished                  int
	conflicts                        int // files that will conflict between members
	messages                         int
	paused                           bool
}

func (m *Model) groupStats(gi int) groupStats {
	g := m.listGroups[gi]
	o := groupStats{worktrees: len(g.members)}
	in := map[int]bool{}
	for _, wt := range g.members {
		in[wt] = true
		s := m.snap.States[wt]
		o.files += len(s.Files)
		o.added += s.Added
		o.deleted += s.Deleted
		if ses := m.sessionFor(s.Path); ses != nil {
			switch {
			case ses.running:
				o.working++
			case len(ses.pending) > 0:
				o.waiting++
			case ses.failed:
				o.failed++
			}
		}
	}
	files := map[string]bool{}
	for _, p := range m.conflictPairs() {
		if in[p.A] && in[p.B] {
			for _, f := range p.Files {
				files[f.Path] = true
			}
		}
	}
	o.conflicts = len(files)
	if p, grp := m.groupPlan(gi); p != nil {
		for _, t := range p.Members(grp.ID) {
			o.tasks++
			if t.Status == plan.Finished {
				o.finished++
			}
		}
		if log, ok := m.groupLog(p, grp.ID); ok {
			if gs := m.groups[log.Dir]; gs != nil {
				o.messages, o.paused = len(gs.msgs), gs.paused
			}
		}
	}
	return o
}

// groupBadges are the header's figures, most urgent first, so the ones
// that don't fit are the least important.
func (m *Model) groupBadges(o groupStats) []string {
	st := m.st
	out := []string{st.muted.Render(plural(o.worktrees, "worktree"))}
	if o.conflicts > 0 {
		out = append(out, st.conflict.Render("✗ "+plural(o.conflicts, "conflict")))
	}
	if o.waiting > 0 {
		out = append(out, st.warn.Bold(true).Render(fmt.Sprintf("✦ %d needs you", o.waiting)))
	}
	if o.failed > 0 {
		out = append(out, st.del.Render(fmt.Sprintf("✦ %d failed", o.failed)))
	}
	if o.working > 0 {
		out = append(out, st.accent.Render(fmt.Sprintf("✦ %d working", o.working)))
	}
	if o.finished > 0 {
		out = append(out, st.add.Render(fmt.Sprintf("✓ %d/%d done", o.finished, o.tasks)))
	}
	if o.paused {
		out = append(out, st.warn.Render("paused"))
	}
	if o.messages > 0 {
		out = append(out, st.faint.Render(plural(o.messages, "message")))
	}
	return out
}

// groupColor is the colour of a group's border: what most needs the user,
// or a quiet line when nothing does.
func (m *Model) groupColor(gi int, o groupStats) color.Color {
	p := m.st.p
	switch {
	case o.conflicts > 0:
		return p.conflict
	case o.waiting > 0:
		return p.warn
	case o.failed > 0:
		return p.del
	case o.working > 0:
		return p.accent
	case m.cursor < len(m.rows) && m.rows[m.cursor].grp == gi:
		return p.muted
	}
	return p.faint
}

// groupHeader is a group's line in the list: the top of its box when it is
// unfolded and there's room for one, otherwise a line of its own.
func (m *Model) groupHeader(gi int, o groupStats, width int, selected bool) string {
	st := m.st
	_, grp := m.groupPlan(gi)
	folded := m.collapsed[m.listGroups[gi].key()]
	chev := "▾"
	if folded {
		chev = "▸"
	}
	nameStyle := st.bold
	if selected {
		nameStyle = st.bold.Foreground(st.p.accent)
	}
	left := st.faint.Render(chev) + " " + groupIcon(grp) + " " + nameStyle.Render(grp.Name)
	right := ""
	if o.added > 0 || o.deleted > 0 {
		right = st.add.Render(fmt.Sprintf("+%s", compact(o.added))) + " " + st.del.Render(fmt.Sprintf("−%s", compact(o.deleted)))
	}
	boxed := !folded && width >= groupBoxWidth
	// " ╭─ " left " " ─… " " right " ─╮ ", with at least one dash.
	room := width - ansi.StringWidth(right) - 3
	if boxed {
		room = width - ansi.StringWidth(right) - 11
	}
	for _, b := range m.groupBadges(o) {
		if ansi.StringWidth(left)+2+ansi.StringWidth(b) > room {
			break
		}
		left += "  " + b
	}
	bg := m.rowBg(selected)
	if !boxed {
		return fill(spread(" "+left, right+" ", width), width, bg)
	}
	left = ansi.Truncate(left, max(room, 1), "…")
	tail := ""
	if right != "" {
		tail = " " + right
	}
	b := lipgloss.NewStyle().Foreground(m.groupColor(gi, o))
	dashes := max(width-9-ansi.StringWidth(left)-ansi.StringWidth(tail), 1)
	line := " " + b.Render("╭─") + " " + left + " " + b.Render(strings.Repeat("─", dashes)) + tail + " " + b.Render("─╮") + " "
	return fill(line, width, bg)
}

// groupSide frames a line inside a group: the width left for the line, and
// what draws the border around it.
func (m *Model) groupSide(c color.Color, width int) (int, func(string) string) {
	b := lipgloss.NewStyle().Foreground(c)
	if width < groupBoxWidth {
		return width - 2, func(s string) string { return " " + b.Render("┃") + fill(s, width-2, nil) }
	}
	return width - 4, func(s string) string { return " " + b.Render("│") + fill(s, width-4, nil) + b.Render("│") + " " }
}

// groupBottom closes a group's box.
func (m *Model) groupBottom(c color.Color, width int) string {
	b := lipgloss.NewStyle().Foreground(c)
	return " " + b.Render("╰"+strings.Repeat("─", max(width-4, 0))+"╯") + " "
}

// groupSelected reports whether the cursor is on a group's header.
func (m *Model) groupSelected() bool {
	r, ok := m.cursorRow()
	return ok && r.kind == rowGroup
}

// groupRow is the list row of a group's header.
func (m *Model) groupRow(gi int) int {
	for i, r := range m.rows {
		if r.kind == rowGroup && r.grp == gi {
			return i
		}
	}
	return m.cursor
}

func (m *Model) toggleGroup(gi int) tea.Cmd {
	k := m.listGroups[gi].key()
	m.collapsed[k] = !m.collapsed[k]
	m.lastSel = k
	m.rebuildRows()
	return m.selectionChanged(false)
}

// unfold makes sure the worktree at path shows in the list with its files,
// opening its group too.
func (m *Model) unfold(path string) {
	delete(m.collapsed, path)
	if p, t := m.taskFor(path); t != nil && t.Group != "" {
		delete(m.collapsed, groupKey(p.ID, t.Group))
	}
}

func (m *Model) onGroupRowKey(msg tea.KeyPressMsg, r row) tea.Cmd {
	k := m.keys
	folded := m.collapsed[m.listGroups[r.grp].key()]
	switch {
	case key.Matches(msg, k.Collapse):
		if !folded {
			return m.toggleGroup(r.grp)
		}
	case key.Matches(msg, k.Expand):
		if folded {
			return m.toggleGroup(r.grp)
		}
		return m.move(1)
	case key.Matches(msg, k.Open), msg.String() == "space":
		return m.toggleGroup(r.grp)
	}
	return nil
}

// groupSummaryLines describe a group in the details pane: its members and
// their agents, how they'll merge, and the latest of their chat.
func (m *Model) groupSummaryLines(gi, width int) []string {
	st := m.st
	g := m.listGroups[gi]
	p, grp := m.groupPlan(gi)
	o := m.groupStats(gi)
	label := func(l string) string { return "  " + st.muted.Render(fmt.Sprintf("%-16s", l)) }
	trunc := func(s string, w int) string { return ansi.Truncate(s, max(w, 1), "…") }

	out := []string{"", "  " + groupIcon(grp) + " " + st.bold.Foreground(st.p.accent).Render(trunc(grp.Name, width-8))}
	if p != nil {
		out = append(out, "  "+st.faint.Render(trunc("group in the plan "+p.Title, width-4)))
		if p.Orchestrated {
			out = append(out, "  "+st.accent.Render("✦ ")+st.muted.Render(trunc("the planner watches these agents for you", width-6)))
		}
	}
	out = append(out, "")
	if o.tasks > 0 {
		out = append(out, label("Tasks")+st.text.Render(fmt.Sprintf("%d of %d finished", o.finished, o.tasks)))
	}
	if o.files == 0 {
		out = append(out, label("Changes")+st.muted.Render("none yet"))
	} else {
		out = append(out, label("Changes")+st.text.Render(plural(o.files, "file"))+"  "+st.add.Render(fmt.Sprintf("+%d", o.added))+" "+st.del.Render(fmt.Sprintf("−%d", o.deleted)))
	}
	var agents []string
	if o.waiting > 0 {
		agents = append(agents, st.warn.Bold(true).Render(fmt.Sprintf("%d needs you", o.waiting)))
	}
	if o.working > 0 {
		agents = append(agents, st.accent.Render(fmt.Sprintf("%d working", o.working)))
	}
	if o.failed > 0 {
		agents = append(agents, st.del.Render(fmt.Sprintf("%d failed", o.failed)))
	}
	if len(agents) > 0 {
		out = append(out, label("Agents")+strings.Join(agents, st.faint.Render(" · ")))
	}
	if p != nil {
		if u := m.groupUsage(p, grp.ID); u != "" {
			out = append(out, label("Tokens")+u+st.faint.Render("  T for details"))
		}
	}

	// Members, including tasks whose worktree isn't made yet.
	out = append(out, "", "  "+st.bold.Render("Members"))
	if p != nil {
		for _, t := range p.Members(grp.ID) {
			status := st.faint.Render(string(t.Status))
			if badge := m.agentBadge(t.Path); t.Path != "" && badge != "" {
				status = badge
			}
			if m.checkIn(t.Path) != nil {
				status = st.warn.Render("testing…")
			}
			right := ""
			if t.Path != "" {
				if wt := m.snap.IndexOf(t.Path); wt < 0 {
					right = st.faint.Render("folder gone")
				} else if s := m.snap.States[wt]; len(s.Files) > 0 {
					right = stat(st, s.Added, s.Deleted)
				} else {
					right = st.faint.Render("no changes")
				}
			}
			out = append(out, spread("  "+m.taskIcon(t)+" "+st.text.Render(t.Branch)+"  "+status, right+"  ", width))
			if t.Title != "" {
				out = append(out, "    "+st.faint.Render(trunc(t.Title, width-6)))
			}
			if t.Status == plan.Failed && t.Error != "" {
				out = append(out, "    "+st.del.Render(trunc(t.Error, width-6)))
			}
		}
	}

	// How the members will merge, with each other and with everyone else.
	in := map[int]bool{}
	for _, wt := range g.members {
		in[wt] = true
	}
	var inside, outside []string
	for _, pr := range m.conflictPairs() {
		if !in[pr.A] && !in[pr.B] {
			continue
		}
		a, b := m.snap.States[pr.A].Name(), m.snap.States[pr.B].Name()
		if !in[pr.A] {
			a, b = b, a
		}
		line := "  " + st.conflict.Render("✗ ") + st.text.Render(a) + st.faint.Render(" ↔ ") + st.text.Render(b) + st.faint.Render("  "+plural(len(pr.Files), "file"))
		if len(pr.Files) > 0 {
			line += st.faint.Render(": ") + st.muted.Render(pr.Files[0].Path)
			if len(pr.Files) > 1 {
				line += st.faint.Render(fmt.Sprintf(" +%d", len(pr.Files)-1))
			}
		}
		if in[pr.A] && in[pr.B] {
			inside = append(inside, trunc(line, width-1))
		} else {
			outside = append(outside, trunc(line, width-1))
		}
	}
	shared := 0
	for _, idx := range m.snap.Overlaps {
		n := 0
		for _, i := range idx {
			if in[i] {
				n++
			}
		}
		if n > 1 {
			shared++
		}
	}
	out = append(out, "", "  "+st.bold.Render("Merging"))
	switch {
	case len(inside) > 0:
		out = append(out, "  "+st.conflict.Render(fmt.Sprintf("Members will conflict in %s", plural(o.conflicts, "file"))))
		out = append(out, inside...)
	case shared > 0:
		out = append(out, "  "+st.warn.Render("⚠ ")+st.muted.Render(plural(shared, "file")+" changed by more than one member, no conflicts predicted"))
	default:
		out = append(out, "  "+st.add.Render("✓ ")+st.muted.Render("members don't change the same files"))
	}
	if len(outside) > 0 {
		out = append(out, "  "+st.conflict.Render("With other worktrees"))
		out = append(out, outside...)
	}

	// The latest of the chat.
	out = append(out, "", "  "+st.bold.Render("Group chat")+st.faint.Render("  "+plural(o.messages, "message")))
	var gs *groupState
	if p != nil {
		if log, ok := m.groupLog(p, grp.ID); ok {
			gs = m.groups[log.Dir]
		}
	}
	if gs == nil || len(gs.msgs) == 0 {
		out = append(out, "  "+st.muted.Render("No messages yet."))
	} else {
		for _, msg := range gs.msgs[max(len(gs.msgs)-5, 0):] {
			from := st.text.Render(msg.From)
			switch msg.From {
			case group.User:
				from = st.accent.Render("you")
			case group.Grove:
				from = st.warn.Render("grove")
			}
			if msg.To != "" {
				from += st.faint.Render(" → " + msg.To)
			}
			head := "  " + st.faint.Render(msg.Time.Local().Format("15:04")) + " " + from + "  "
			out = append(out, head+st.muted.Render(trunc(firstLine(msg.Text), width-ansi.StringWidth(head)-1)))
		}
	}
	if gs != nil && gs.paused {
		out = append(out, "  "+st.warn.Render(trunc(fmt.Sprintf("Paused: agents were woken %d times in a row. Post to let them go on.", gs.wakeups), width-3)))
	}

	out = append(out, "", "  "+m.hints([][2]string{{"t", "group chat"}, {"enter", "fold"}, {"[ ]", "prev / next"}}))
	return out
}

// groupTitle is a group's name with its icon, for pane titles and toasts.
func groupTitle(g plan.Group) string { return groupIcon(g) + " " + g.Name }
