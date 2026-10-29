package ui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/charmbracelet/x/ansi"
)

// planReview is the proposed plan being reviewed. It edits a copy; the
// draft is updated when the review closes or the plan starts.
type planReview struct {
	p        *plan.Plan
	cursor   int
	edit     *taskEdit
	naming   *textinput.Model // renaming the selected task's group
	problems []string
	checking bool
}

// taskEdit is the form for one task.
type taskEdit struct {
	focus  int // editTitle, editBranch or editBrief
	title  textinput.Model
	branch textinput.Model
	brief  textarea.Model
}

const (
	editTitle = iota
	editBranch
	editBrief
)

func newReview(p *plan.Plan) *planReview { return &planReview{p: p.Clone()} }

func (r *planReview) task() *plan.Task {
	if r.cursor < 0 || r.cursor >= len(r.p.Tasks) {
		return nil
	}
	return &r.p.Tasks[r.cursor]
}

func (m *Model) onReviewKey(msg tea.KeyPressMsg) tea.Cmd {
	r := m.review
	k := msg.String()
	if k == "ctrl+c" {
		return m.quitNow()
	}
	switch {
	case r.checking:
		return nil
	case r.edit != nil:
		return m.onTaskEditKey(msg)
	case r.naming != nil:
		switch k {
		case "esc":
			r.naming = nil
		case "enter":
			if g, ok := r.p.Group(r.task().Group); ok {
				if icon, name := splitGroupIcon(r.naming.Value()); name != "" {
					g.Icon, g.Name = icon, name
				}
			}
			r.naming = nil
		default:
			var cmd tea.Cmd
			*r.naming, cmd = r.naming.Update(msg)
			return cmd
		}
		return nil
	}
	r.problems = nil
	switch k {
	case "esc", "q":
		return m.closeReview()
	case "up", "k":
		r.cursor = max(0, r.cursor-1)
	case "down", "j":
		r.cursor = min(len(r.p.Tasks)-1, r.cursor+1)
	case "enter", "e":
		if t := r.task(); t != nil {
			r.edit = m.newTaskEdit(t)
			return r.edit.title.Focus()
		}
	case "g":
		if t := r.task(); t != nil {
			cycleGroup(r.p, t)
		}
	case "r":
		if t := r.task(); t != nil {
			if g, ok := r.p.Group(t.Group); ok {
				ti := textinput.New()
				ti.Prompt = ""
				ti.SetStyles(textinput.DefaultStyles(m.dark))
				name := g.Name
				if icon := groupIcon(*g); icon == g.Icon {
					name = icon + " " + name
				}
				ti.SetValue(name)
				ti.CursorEnd()
				r.naming = &ti
				return r.naming.Focus()
			}
		}
	case "d", "delete":
		if len(r.p.Tasks) > 0 {
			r.p.Tasks = append(r.p.Tasks[:r.cursor], r.p.Tasks[r.cursor+1:]...)
			dropEmptyGroups(r.p)
			r.cursor = max(0, min(r.cursor, len(r.p.Tasks)-1))
		}
	case "o":
		// Who watches the agents; kept for plans the planner proposes again.
		r.p.Orchestrated = !r.p.Orchestrated
		m.planWatch = config.WatchYou
		if r.p.Orchestrated {
			m.planWatch = config.WatchPlanner
		}
	case "s":
		r.checking = true
		return tea.Batch(m.checkPlan(r.p), m.startAnimating())
	}
	return nil
}

// closeReview keeps the edits in the draft, to come back to with ctrl+o.
func (m *Model) closeReview() tea.Cmd {
	r := m.review
	m.review = nil
	for i, p := range m.plans {
		if p.ID == r.p.ID && p.Status == plan.Draft {
			m.plans[i] = r.p
			return m.savePlan(r.p)
		}
	}
	return nil
}

func (m *Model) newTaskEdit(t *plan.Task) *taskEdit {
	e := &taskEdit{title: textinput.New(), branch: textinput.New(), brief: textarea.New()}
	for _, ti := range []*textinput.Model{&e.title, &e.branch} {
		ti.Prompt = ""
		ti.SetStyles(textinput.DefaultStyles(m.dark))
	}
	e.title.SetValue(t.Title)
	e.branch.SetValue(t.Branch)
	e.brief.Prompt = ""
	e.brief.ShowLineNumbers = false
	e.brief.SetStyles(chatInputStyles(m.dark))
	e.brief.MaxContentHeight = 5000
	e.brief.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "ctrl+j", "alt+enter"))
	e.brief.SetValue(t.Brief)
	return e
}

func (e *taskEdit) focusOn(i int) tea.Cmd {
	e.focus = (i + 3) % 3
	e.title.Blur()
	e.branch.Blur()
	e.brief.Blur()
	switch e.focus {
	case editTitle:
		return e.title.Focus()
	case editBranch:
		return e.branch.Focus()
	}
	return e.brief.Focus()
}

func (m *Model) onTaskEditKey(msg tea.KeyPressMsg) tea.Cmd {
	r := m.review
	e := r.edit
	switch msg.String() {
	case "esc":
		r.edit = nil
		return nil
	case "tab":
		return e.focusOn(e.focus + 1)
	case "shift+tab":
		return e.focusOn(e.focus - 1)
	case "enter":
		t := r.task()
		t.Title = strings.TrimSpace(e.title.Value())
		t.Branch = strings.TrimSpace(e.branch.Value())
		t.Brief = strings.TrimSpace(e.brief.Value())
		r.edit = nil
		return nil
	}
	return e.update(msg)
}

// update sends a key or paste to the focused field.
func (e *taskEdit) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch e.focus {
	case editTitle:
		e.title, cmd = e.title.Update(msg)
	case editBranch:
		e.branch, cmd = e.branch.Update(msg)
	default:
		e.brief, cmd = e.brief.Update(msg)
	}
	return cmd
}

// cycleGroup moves a task to the next group: none, each existing group in
// turn, then a new group of its own.
func cycleGroup(p *plan.Plan, t *plan.Task) {
	opts := []string{""}
	for _, g := range p.Groups {
		opts = append(opts, g.ID)
	}
	i := 0
	for j, o := range opts {
		if o == t.Group {
			i = j
		}
	}
	if i+1 < len(opts) {
		t.Group = opts[i+1]
	} else if t.Group != "" && len(p.Members(t.Group)) == 1 {
		// Already alone in its own group: back to isolated.
		t.Group = ""
	} else {
		n := len(p.Groups) + 1
		id := fmt.Sprintf("group-%d", n)
		for _, ok := p.Group(id); ok; _, ok = p.Group(id) {
			n++
			id = fmt.Sprintf("group-%d", n)
		}
		p.Groups = append(p.Groups, plan.Group{ID: id, Name: fmt.Sprintf("Group %d", n)})
		t.Group = id
	}
	dropEmptyGroups(p)
}

// dropEmptyGroups removes groups no task belongs to. A group of one stays
// while editing, since a second task may join it; starting the plan
// ungroups it.
func dropEmptyGroups(p *plan.Plan) {
	var keep []plan.Group
	for _, g := range p.Groups {
		if len(p.Members(g.ID)) > 0 {
			keep = append(keep, g)
		}
	}
	p.Groups = keep
}

// reviewModal draws the plan under review.
func (m *Model) reviewModal() string {
	r, st := m.review, m.st
	width := min(m.width-4, 100)
	inner := width - 6
	room := max(m.height-12, 4)
	trunc := func(s string, w int) string { return ansi.Truncate(s, max(w, 1), "…") }

	head := st.bold.Render("Review plan") + st.faint.Render(" · ") + st.bold.Foreground(st.p.accent).Render(trunc(r.p.Title, inner-14))
	sub := st.muted.Render(fmt.Sprintf("%s · %s", plural(len(r.p.Tasks), "task"), plural(len(r.p.Groups), "group")))
	if r.p.Base != "" {
		sub += st.muted.Render(" · from " + r.p.Base)
	}
	sub += st.faint.Render(" · nothing is created until you start")
	watch := st.muted.Render("Watched by ") + st.text.Render("you") + st.faint.Render(": commands, questions and group chats wait for you")
	if r.p.Orchestrated {
		watch = st.muted.Render("Watched by ") + st.accent.Render("the planner") + st.faint.Render(": it approves safe commands, answers what it can, follows group chats")
	}
	lines := []string{head, trunc(sub, inner), trunc(watch, inner), ""}

	var keys [][2]string
	switch {
	case r.edit != nil:
		e := r.edit
		label := func(i int, s string) string {
			if e.focus == i {
				return st.accent.Bold(true).Render(fmt.Sprintf("%-7s", s))
			}
			return st.muted.Render(fmt.Sprintf("%-7s", s))
		}
		e.title.SetWidth(max(inner-9, 10))
		e.branch.SetWidth(max(inner-9, 10))
		e.brief.SetWidth(max(inner-2, 10))
		e.brief.SetHeight(max(min(room-6, 12), 3))
		lines = append(lines,
			st.bold.Render(fmt.Sprintf("Edit task %d of %d", r.cursor+1, len(r.p.Tasks))), "",
			label(editTitle, "Title")+"  "+e.title.View(),
			label(editBranch, "Branch")+"  "+e.branch.View(),
			label(editBrief, "Brief"),
		)
		for _, l := range strings.Split(e.brief.View(), "\n") {
			lines = append(lines, "  "+l)
		}
		keys = [][2]string{{"tab", "next field"}, {"enter", "save"}, {"shift+enter", "new line"}, {"esc", "cancel"}}

	case len(r.p.Tasks) == 0:
		lines = append(lines, st.muted.Render("Every task was removed. Close this and ask the planner again."))
		keys = [][2]string{{"esc", "close"}}

	default:
		var rows []string
		cur := 0
		for i := range r.p.Tasks {
			t := &r.p.Tasks[i]
			mark := "  "
			if i == r.cursor {
				mark = st.accent.Render("› ")
				cur = len(rows)
			}
			grp := st.faint.Render("isolated")
			if g, ok := r.p.Group(t.Group); ok {
				grp = st.warn.Render(groupTitle(*g))
			}
			title := st.text.Render(t.Title)
			if i == r.cursor {
				title = st.bold.Render(t.Title)
			}
			rows = append(rows, trunc(mark+st.faint.Render(fmt.Sprintf("%d. ", i+1))+title+"  "+st.accent.Render(t.Branch)+"  "+grp, inner))
			if i == r.cursor {
				brief := lipgloss.NewStyle().Width(max(inner-5, 10)).Render(t.Brief)
				bl := strings.Split(brief, "\n")
				if len(bl) > 6 {
					bl = append(bl[:5], "…")
				}
				for _, l := range bl {
					rows = append(rows, "     "+st.muted.Render(l))
				}
			}
		}
		listRoom := max(room-len(r.problems)-2, 3)
		start := max(0, min(cur-listRoom/3, len(rows)-listRoom))
		lines = append(lines, rows[start:min(len(rows), start+listRoom)]...)
		if g, ok := r.p.Group(r.task().Group); ok && r.naming != nil {
			lines = append(lines, "", st.muted.Render("Name for "+g.Name+": ")+r.naming.View(),
				st.faint.Render(trunc("Start with an emoji to give the group an icon, e.g. 🛒 Checkout", inner)))
			keys = [][2]string{{"enter", "rename"}, {"esc", "cancel"}}
		} else {
			keys = [][2]string{{"↑↓", "select"}, {"enter", "edit"}, {"g", "group"}, {"r", "rename group"}, {"o", "who watches"}, {"d", "remove"}, {"s", "start"}, {"esc", "close"}}
		}
	}
	if len(r.problems) > 0 {
		lines = append(lines, "", st.del.Bold(true).Render("Fix these before starting:"))
		for i, p := range r.problems {
			if i == 4 && len(r.problems) > 5 {
				lines = append(lines, st.del.Render(fmt.Sprintf("  … %d more", len(r.problems)-4)))
				break
			}
			lines = append(lines, st.del.Render(trunc("  "+p, inner)))
		}
	}
	if r.checking {
		lines = append(lines, "", st.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)])+st.muted.Render(" Checking branches and folders…"))
		keys = nil
	}
	if keys != nil {
		lines = append(lines, "", trunc(m.hints(keys), inner))
	}
	return st.modal.Padding(1, 2).Width(width).Render(strings.Join(lines, "\n"))
}
