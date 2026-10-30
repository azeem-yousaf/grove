package ui

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/usage"
)

// Token usage: every turn an agent finishes becomes one record in the usage
// ledger (m.usage), attributed to the plan, task and group it worked for.
// The running turn's figure (session.live) is only shown, never stored,
// except when grove closes mid-turn: then it is saved with the session and
// recorded as cut short when the session is loaded again.

// turnAttr is what a turn worked for.
type turnAttr struct {
	Plan      string     `json:"plan,omitempty"`
	PlanTitle string     `json:"plan_title,omitempty"`
	Task      string     `json:"task,omitempty"`
	Group     string     `json:"group,omitempty"`
	Kind      usage.Kind `json:"kind,omitempty"` // why the turn happened, fixed when it starts
}

// usageReload is how often the usage screen rereads the ledger, which other
// grove windows may have added to.
const usageReload = 5 * time.Second

// newUID is grove's id for a new agent.
func newUID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// usageFailed reports a ledger error once, until a different one happens.
func (m *Model) usageFailed(err error) tea.Cmd {
	if err == nil {
		return nil
	}
	if m.usageErr != nil && m.usageErr.Error() == err.Error() {
		return nil
	}
	m.usageErr = err
	return m.notify("Couldn't record token usage: "+err.Error(), toastErr)
}

// setSessionID notes the agent's own session; the session total it reported
// belongs to the old one, and so do the planner's rules unless this is the
// conversation they started.
func (s *session) setSessionID(id string) {
	if id != s.id {
		s.sessionTotal = usage.Tokens{}
		if s.id != "" {
			s.rulesSent = ""
		}
	}
	s.id = id
}

// startTurn numbers s's new turn and notes what it is for.
func (m *Model) startTurn(s *session) tea.Cmd {
	s.turns++
	// Two grove windows on one repository load the same session, uid and
	// counter, so the process's nonce keeps their turn IDs apart.
	s.turnID = fmt.Sprintf("%s/%s/%d", s.uid, m.turnNonce, s.turns)
	s.live, s.liveSeen = usage.Tokens{}, false
	planID := ""
	if s.planner {
		// A group planner works for its plan; the lead only for the plan
		// grove asked it about (orchestrateOne). Anything else, such as
		// the user's message in its chat, is planning: drafting turns that
		// go to the draft they lead to, even while it watches a plan.
		planID = cmp.Or(s.crewPlan, s.nextPlan)
		s.nextPlan = ""
	}
	s.turnAttr = m.attrFor(s, planID)
	s.turnAttr.Kind = turnKind(s)
	s.nextKind = ""
	return m.registerAgent(s)
}

// turnKind is why s's next turn happens: what its caller set in s.nextKind
// just before send, else a message from the user or the plan, which for the
// planner means planning.
func turnKind(s *session) usage.Kind {
	switch {
	case s.nextKind != "":
		return s.nextKind
	case s.planner:
		return usage.KindPlan
	}
	return usage.KindWork
}

// sendAs starts a turn of kind k.
func (m *Model) sendAs(s *session, k usage.Kind, text string) tea.Cmd {
	s.nextKind = k
	return m.send(s, text)
}

// attrFor is what s's turn works for right now. The planner's plan is
// chosen when its turn starts (planID); a worktree agent's comes from its
// task, or the plan its worktree is the merge of.
func (m *Model) attrFor(s *session, planID string) turnAttr {
	if s.planner {
		a := turnAttr{Plan: planID, Group: s.crewGroup}
		if p := m.planByID(planID); p != nil {
			a.PlanTitle = p.Title
		}
		return a
	}
	if p, t := m.taskFor(s.path); t != nil {
		return turnAttr{Plan: p.ID, PlanTitle: p.Title, Task: t.ID, Group: t.Group}
	}
	if p := m.mergedInto(s.path); p != nil {
		return turnAttr{Plan: p.ID, PlanTitle: p.Title}
	}
	return turnAttr{}
}

// branchOf is the branch s works on, as the latest scan shows it.
func (m *Model) branchOf(s *session) string {
	if s.planner {
		return ""
	}
	if i := m.snapIndex(s.path); i >= 0 {
		return m.snap.States[i].Name()
	}
	return s.name
}

// registerAgent tells the ledger about s on its first turn, and again when
// its agent, model or branch changed.
func (m *Model) registerAgent(s *session) tea.Cmd {
	if m.usage == nil {
		return nil
	}
	branch := m.branchOf(s)
	key := string(s.kind) + "\x00" + s.model + "\x00" + branch
	if key == s.agentKey {
		return nil
	}
	created := s.created
	if created.IsZero() {
		created = time.Now()
	}
	info := usage.AgentInfo{
		ID: s.uid, Kind: string(s.kind), Model: s.model, Path: s.path, Branch: branch,
		Planner: s.planner, Created: created,
	}
	// Assumed written; a failure clears it so the next turn tries again.
	s.agentKey = key
	l := m.usage
	return func() tea.Msg {
		if err := l.SetAgent(info); err != nil {
			return usageWrittenMsg{uid: info.ID, agentFailed: true, err: err}
		}
		return nil
	}
}

// usageWrittenMsg reports ledger writes for the agent uid, which run in
// tea.Cmds because a write may wait for another grove's lock.
type usageWrittenMsg struct {
	uid         string
	flushed     bool     // from flushUsage
	written     []string // turn IDs now in the ledger
	assigned    int      // assignments written, from the front of the queue
	agentFailed bool     // SetAgent failed
	err         error
}

// sessionByUID finds the agent with grove id uid, removed ones still
// writing their usage included.
func (m *Model) sessionByUID(uid string) *session {
	for _, s := range m.allSessions() {
		if s.uid == uid {
			return s
		}
	}
	for _, s := range m.usageLeft {
		if s.uid == uid {
			return s
		}
	}
	return nil
}

// keepUsage holds on to a removed agent until its queued turns and
// assignments are in the ledger, including those a flush already in flight
// is writing: its result needs the session to find.
func (m *Model) keepUsage(s *session) {
	if m.usage != nil && (s.writing || len(s.unrecorded)+len(s.unassigned) > 0) && !slices.Contains(m.usageLeft, s) {
		m.usageLeft = append(m.usageLeft, s)
	}
}

// takeUsage moves from's usage not yet in the ledger to to, when a loaded
// session loses to one already there. Its turns keep their agent; the
// queue writes all turns before any assignment.
func (m *Model) takeUsage(to, from *session) {
	to.unrecorded = append(to.unrecorded, from.unrecorded...)
	to.unassigned = append(to.unassigned, from.unassigned...)
	if to.planner && to.crewGroup == "" {
		to.drafting = append(to.drafting, from.drafting...)
	}
	from.unrecorded, from.unassigned, from.drafting = nil, nil, nil
}

func (m *Model) onUsageWritten(msg usageWrittenMsg) tea.Cmd {
	s := m.sessionByUID(msg.uid)
	if s == nil {
		return m.usageFailed(msg.err)
	}
	if msg.agentFailed {
		s.agentKey = ""
	}
	if !msg.flushed {
		return m.usageFailed(msg.err)
	}
	s.writing = false
	s.unrecorded = slices.DeleteFunc(s.unrecorded, func(t usage.Turn) bool { return slices.Contains(msg.written, t.ID) })
	s.unassigned = s.unassigned[min(msg.assigned, len(s.unassigned)):]
	if msg.err != nil {
		return m.usageFailed(msg.err) // the clock tries again
	}
	if len(s.unrecorded)+len(s.unassigned) == 0 {
		m.usageLeft = slices.DeleteFunc(m.usageLeft, func(o *session) bool { return o == s })
	}
	return m.flushUsage(s) // anything queued meanwhile
}

// flushUsage writes s's turns not yet in the ledger, then its queued
// assignments, so an assignment never comes before its turns. One flush
// runs at a time per agent. The clock calls it again while anything is left
// (retryUsage), and the ledger ignores a turn it already has, so retrying
// never counts one twice.
func (m *Model) flushUsage(s *session) tea.Cmd {
	if m.usage == nil || s.writing || len(s.unrecorded)+len(s.unassigned) == 0 {
		return nil
	}
	s.writing = true
	l, uid := m.usage, s.uid
	turns, assigns := slices.Clone(s.unrecorded), slices.Clone(s.unassigned)
	return func() tea.Msg {
		msg := usageWrittenMsg{uid: uid, flushed: true}
		for _, t := range turns {
			if _, err := l.Add(t); err != nil {
				msg.err = err
				return msg
			}
			msg.written = append(msg.written, t.ID)
		}
		for _, a := range assigns {
			if err := l.Assign(a); err != nil {
				msg.err = err
				return msg
			}
			msg.assigned++
		}
		return msg
	}
}

// retryUsage writes turns left over from failed writes, or from loading.
func (m *Model) retryUsage() tea.Cmd {
	var cmds []tea.Cmd
	for _, s := range append(m.allSessions(), m.usageLeft...) {
		cmds = append(cmds, m.flushUsage(s))
	}
	return tea.Batch(cmds...)
}

// endTurn records s's turn as ev ended it, once.
func (m *Model) endTurn(s *session, ev agent.Event) tea.Cmd {
	id := s.turnID
	attr := s.turnAttr
	s.turnID, s.live, s.liveSeen = "", usage.Tokens{}, false
	if id == "" {
		return nil
	}
	if s.planner {
		attr = m.attrFor(s, attr.Plan)
		if attr.Plan == "" {
			s.drafting = append(s.drafting, id)
		}
	} else {
		attr = m.attrFor(s, "")
	}
	attr.Kind = s.turnAttr.Kind
	// The provider's session total is good whenever it came, even for a
	// Partial turn (one without a base that fits); a turn without one
	// leaves the old one stale: passing it back would count this turn twice.
	s.sessionTotal = ev.SessionTotal
	m.record(s, usage.Turn{
		ID: id, Agent: s.uid, Time: time.Now(), Tokens: ev.Tokens, Status: ev.TokenStatus, Failed: ev.Err != nil,
		Plan: attr.Plan, PlanTitle: attr.PlanTitle, Task: attr.Task, Group: attr.Group, Kind: attr.Kind,
	})
	return m.flushUsage(s)
}

// record queues t to be written with s's other unrecorded turns, which are
// saved with the session until the ledger has them.
func (m *Model) record(s *session, t usage.Turn) {
	if m.usage != nil {
		s.unrecorded = append(s.unrecorded, t)
	}
}

// abandonTurn records the turn of an agent that is going away mid-turn
// (its worktree is being removed), whose end grove won't see.
func (m *Model) abandonTurn(s *session) tea.Cmd {
	if !s.running || s.turnID == "" {
		return nil
	}
	status := usage.Unknown
	if s.liveSeen {
		status = usage.Partial
	}
	return m.endTurn(s, agent.Event{Kind: agent.EvDone, Tokens: s.live, TokenStatus: status})
}

// recordCutShort queues the turn a loaded session was saved in the middle
// of, reporting whether there was one; the clock writes it. The ledger
// ignores a turn it already has, so loading the same file twice records it
// once.
func (m *Model) recordCutShort(s *session) bool {
	c := s.cutShort
	if c == nil {
		return false
	}
	s.cutShort = nil
	s.sessionTotal = usage.Tokens{} // it includes the turn cut short, or not: unknown
	t := usage.Turn{
		ID: c.ID, Agent: s.uid, Time: time.Now(), Status: usage.Unknown,
		Plan: c.Attr.Plan, PlanTitle: c.Attr.PlanTitle, Task: c.Attr.Task, Group: c.Attr.Group, Kind: c.Attr.Kind,
	}
	if c.Tokens != nil {
		t.Tokens, t.Status = *c.Tokens, usage.Partial
	}
	if s.planner && c.Attr.Plan == "" {
		s.drafting = append(s.drafting, c.ID)
	}
	m.record(s, t)
	return true
}

// assignDrafting gives the planner's turns without a plan since its previous
// draft to p, the draft they led to. The ledger keeps a turn's first
// assignment, so a draft that replaces another gets only the turns since.
// The plan keeps its ID once started.
func (m *Model) assignDrafting(s *session, p *plan.Plan) tea.Cmd {
	turns := s.drafting
	s.drafting = nil
	if m.usage == nil || len(turns) == 0 {
		return nil
	}
	// Written after the turns themselves, through the same queue.
	s.unassigned = append(s.unassigned, usage.Assignment{Plan: p.ID, PlanTitle: p.Title, Turns: turns})
	return m.flushUsage(s)
}

// liveFor adds up the running turns of the agents keep picks.
func (m *Model) liveFor(keep func(*session) bool) (usage.Tokens, bool) {
	var t usage.Tokens
	running := false
	for _, s := range m.allSessions() {
		if s.running && s.turnID != "" && keep(s) {
			t, running = t.Add(s.live), true
		}
	}
	return t, running
}

// sumOf is the ledger's figure, or nothing without a ledger.
func (m *Model) sumOf(q func(*usage.Ledger) usage.Sum) usage.Sum {
	if m.usage == nil {
		return usage.Sum{}
	}
	return q(m.usage)
}

// notReported is a sum whose turns all came without figures.
func notReported(s usage.Sum) bool { return s.Turns > 0 && s.Unknown == s.Turns }

// usageText is a short figure: "48.1k tok", "≥48.1k tok" when some turns
// were cut short, "usage n/a" when the agent reported nothing, and the
// running turns as "+ 2.3k live". It is "" when there is nothing yet.
func (m *Model) usageText(sum usage.Sum, live usage.Tokens, running bool) string {
	st := m.st
	if sum.Turns == 0 && !running {
		return ""
	}
	var out string
	switch {
	case notReported(sum):
		out = st.faint.Render("usage n/a")
	case sum.Turns > 0 || !running:
		n := usage.Compact(sum.Tokens.Total())
		if !sum.Complete() {
			n = "≥" + n
		}
		out = st.muted.Render(n + " tok")
	}
	if running {
		l := st.accent.Render("+ " + usage.Compact(live.Total()) + " live")
		if out == "" {
			return l
		}
		return out + " " + l
	}
	return out
}

// agentUsage is s's tokens since it was created, for its chat header.
func (m *Model) agentUsage(s *session) string {
	sum := m.sumOf(func(l *usage.Ledger) usage.Sum { return l.Agent(s.uid) })
	return m.usageText(sum, s.live, s.running && s.turnID != "")
}

// agentUsageCost is s's tokens and, for Claude, the session's cost, as
// shown in its chat header and the worktree summary. Claude reports cost
// only when a turn ends, so while one runs the last figure is marked as
// from before it. Codex reports none and gets no cost, never a made-up $0.
func (m *Model) agentUsageCost(s *session) []string {
	var out []string
	if u := m.agentUsage(s); u != "" {
		out = append(out, u)
	}
	if s.cost != "" {
		c := s.cost
		if s.running {
			c += " before this turn"
		}
		out = append(out, m.st.faint.Render(c))
	}
	return out
}

// groupUsage is a plan group's tokens, for its summary.
func (m *Model) groupUsage(p *plan.Plan, groupID string) string {
	sum := m.sumOf(func(l *usage.Ledger) usage.Sum { return l.Group(p.ID, groupID) })
	live, running := m.liveFor(func(s *session) bool {
		return s.turnAttr.Plan == p.ID && s.turnAttr.Group == groupID
	})
	return m.usageText(sum, live, running)
}

// planUsage is a plan's tokens, the planner's share included.
func (m *Model) planUsage(p *plan.Plan) string {
	sum := m.sumOf(func(l *usage.Ledger) usage.Sum { return l.Plan(p.ID) })
	live, running := m.liveFor(func(s *session) bool { return s.turnAttr.Plan == p.ID })
	return m.usageText(sum, live, running)
}

// totalUsage is every agent's tokens.
func (m *Model) totalUsage() string {
	sum := m.sumOf(func(l *usage.Ledger) usage.Sum { return l.Total() })
	live, running := m.liveFor(func(*session) bool { return true })
	return m.usageText(sum, live, running)
}

// The usage screen (T).

func (m *Model) openUsage() tea.Cmd {
	m.usageView, m.usageTop = true, 0
	return m.reloadUsage()
}

// reloadUsage rereads the ledger for the usage screen.
func (m *Model) reloadUsage() tea.Cmd {
	m.usageLoaded = m.now
	if m.usage == nil {
		return nil
	}
	return m.usageFailed(m.usage.Reload())
}

// usageTick rereads the ledger every usageReload while the screen is open.
func (m *Model) usageTick() tea.Cmd {
	if !m.usageView || m.now.Sub(m.usageLoaded) < usageReload {
		return nil
	}
	return m.reloadUsage()
}

func (m *Model) onUsageKey(msg tea.KeyPressMsg) tea.Cmd {
	_, _, paneH := m.layout()
	page := max(paneH-6, 1)
	switch msg.String() {
	case "ctrl+c":
		return m.quitNow()
	case "q":
		return m.quit()
	case "esc", "T":
		m.usageView = false
	case "up", "k":
		m.usageTop--
	case "down", "j":
		m.usageTop++
	case "pgup", "ctrl+u":
		m.usageTop -= page
	case "pgdown", "ctrl+d", "space":
		m.usageTop += page
	case "home", "g":
		m.usageTop = 0
	case "end", "G":
		m.usageTop = 1 << 30
	case "?":
		m.showHelp = true
	}
	m.usageTop = max(m.usageTop, 0) // usagePane clamps the end
	return nil
}

// usageRow is one line of the usage table.
type usageRow struct {
	label   string // styled, indented
	sum     usage.Sum
	live    usage.Tokens
	running bool
	strong  bool // a total, in bold
	heading bool // a section title, without figures
}

// usageCol is one figure column of the usage table.
type usageCol struct {
	title string
	width int
	cell  func(r usageRow) string
}

func (m *Model) usageCols(width int) []usageCol {
	st := m.st
	dash := st.faint.Render("–")
	num := func(r usageRow, n int64) string {
		if r.sum.Turns == 0 || notReported(r.sum) {
			return dash
		}
		return st.text.Render(usage.Compact(n))
	}
	input := usageCol{"INPUT", 8, func(r usageRow) string { return num(r, r.sum.Tokens.Input) }}
	cached := usageCol{"CACHED", 8, func(r usageRow) string {
		if r.sum.Turns > 0 && !notReported(r.sum) && !r.sum.Tokens.HasCache {
			return st.faint.Render("—")
		}
		return num(r, r.sum.Tokens.CacheRead)
	}}
	output := usageCol{"OUTPUT", 8, func(r usageRow) string { return num(r, r.sum.Tokens.Output) }}
	total := usageCol{"TOTAL", 9, func(r usageRow) string {
		switch {
		case r.sum.Turns == 0:
			return dash
		case notReported(r.sum):
			return st.faint.Render("n/a")
		}
		n := usage.Compact(r.sum.Tokens.Total())
		if !r.sum.Complete() {
			n = "≥" + n
		}
		if r.strong {
			return st.bold.Render(n)
		}
		return st.text.Render(n)
	}}
	turns := usageCol{"TURNS", 6, func(r usageRow) string {
		if r.sum.Turns == 0 {
			return dash
		}
		return st.muted.Render(fmt.Sprint(r.sum.Turns))
	}}
	live := usageCol{"LIVE", 9, func(r usageRow) string {
		if !r.running {
			return ""
		}
		return st.accent.Render("+" + usage.Compact(r.live.Total()))
	}}
	// Most useful first; the table keeps as many as leave the label room.
	order := []usageCol{total, live, output, turns, input, cached}
	used := 20
	var keep []usageCol
	for _, c := range order {
		if used+c.width+1 > width {
			continue
		}
		keep = append(keep, c)
		used += c.width + 1
	}
	// Shown in reading order.
	var out []usageCol
	for _, title := range []string{"INPUT", "CACHED", "OUTPUT", "TOTAL", "TURNS", "LIVE"} {
		for _, c := range keep {
			if c.title == title {
				out = append(out, c)
			}
		}
	}
	return out
}

// usageRows lays out the usage table: everything, each plan with its
// groups, tasks and planner, the planner's turns without a plan, and the
// agents by total.
func (m *Model) usageRows() []usageRow {
	st := m.st
	l := m.usage
	q := func(f func(*usage.Ledger) usage.Sum) usage.Sum { return m.sumOf(f) }
	row := func(label string, sum usage.Sum, keep func(*session) bool) usageRow {
		live, running := m.liveFor(keep)
		return usageRow{label: label, sum: sum, live: live, running: running}
	}
	var rows []usageRow
	total := row(st.bold.Render("All agents"), q(func(l *usage.Ledger) usage.Sum { return l.Total() }), func(*session) bool { return true })
	total.strong = true
	rows = append(rows, total, usageRow{})

	// Kinds in their fixed order, with their share of everything.
	sums := map[usage.Kind]usage.Sum{}
	kinds := slices.Clone(usage.Kinds)
	if l != nil {
		for _, k := range l.ByKind() {
			sums[k.Kind] = k.Sum
			if !slices.Contains(kinds, k.Kind) {
				kinds = append(kinds, k.Kind)
			}
		}
	}
	var byKind []usageRow
	for _, kind := range kinds {
		r := row(" "+st.text.Render(string(kind)), sums[kind], func(s *session) bool {
			return cmp.Or(s.turnAttr.Kind, usage.KindUnknown) == kind
		})
		if r.sum.Turns == 0 && !r.running {
			continue
		}
		if r.sum.Turns > 0 {
			r.label += " " + st.faint.Render(usage.Share(r.sum, total.sum))
		}
		byKind = append(byKind, r)
	}
	if len(byKind) > 0 {
		rows = append(rows, usageRow{label: st.muted.Render("By kind"), heading: true})
		rows = append(append(rows, byKind...), usageRow{})
	}

	// Plans newest first: those in the ledger, then any whose only usage
	// is still running.
	type planRef struct{ id, title string }
	var plans []planRef
	seen := map[string]bool{}
	if l != nil {
		ps := l.Plans()
		slices.SortStableFunc(ps, func(a, b usage.PlanSum) int { return b.Last.Compare(a.Last) })
		for _, ps := range ps {
			plans = append(plans, planRef{ps.ID, ps.Title})
			seen[ps.ID] = true
		}
	}
	for _, p := range m.plans {
		if seen[p.ID] {
			continue
		}
		if _, running := m.liveFor(func(s *session) bool { return s.turnAttr.Plan == p.ID }); running {
			plans = append([]planRef{{p.ID, p.Title}}, plans...)
		}
	}
	if len(plans) > 0 {
		rows = append(rows, usageRow{label: st.muted.Render("Plans"), heading: true})
	}
	for _, pr := range plans {
		id := pr.id
		p := m.planByID(id)
		title := pr.title
		status := ""
		if p != nil {
			title = p.Title
			switch p.Status {
			case plan.Done:
				status = st.add.Render(" ✓ done")
			case plan.Review:
				status = st.warn.Render(" in review")
			case plan.Draft:
				status = st.faint.Render(" draft")
			}
		} else {
			status = st.faint.Render(" gone")
		}
		r := row(" "+st.bold.Render("▸ "+title)+status, q(func(l *usage.Ledger) usage.Sum { return l.Plan(id) }),
			func(s *session) bool { return s.turnAttr.Plan == id })
		r.strong = true
		rows = append(rows, r)
		if p != nil {
			task := func(indent string, t plan.Task) usageRow {
				tid := t.ID
				return row(indent+st.text.Render(t.Branch), q(func(l *usage.Ledger) usage.Sum { return l.Task(id, tid) }),
					func(s *session) bool { return s.turnAttr.Plan == id && s.turnAttr.Task == tid })
			}
			for _, g := range p.Groups {
				gid := g.ID
				rows = append(rows, row("   "+st.warn.Render(groupIcon(g)+" "+g.Name), q(func(l *usage.Ledger) usage.Sum { return l.Group(id, gid) }),
					func(s *session) bool { return s.turnAttr.Plan == id && s.turnAttr.Group == gid }))
				for _, t := range p.Members(gid) {
					rows = append(rows, task("      ", *t))
				}
			}
			for _, t := range p.Tasks {
				if t.Group == "" {
					rows = append(rows, task("   ", t))
				}
			}
		}
		rows = append(rows, row("   "+st.accent.Render("✦ planner"), q(func(l *usage.Ledger) usage.Sum { return l.PlannerIn(id) }),
			func(s *session) bool { return s.planner && s.turnAttr.Plan == id }))
	}
	unassigned := row(" "+st.accent.Render("✦ planner")+st.muted.Render(", no plan yet"), q(func(l *usage.Ledger) usage.Sum { return l.PlannerIn("") }),
		func(s *session) bool { return s.planner && s.turnAttr.Plan == "" })
	if unassigned.sum.Turns > 0 || unassigned.running {
		rows = append(rows, unassigned)
	}

	// Agents by total, with the running ones not yet in the ledger.
	var agents []usageRow
	inLedger := map[string]bool{}
	label := func(info usage.AgentInfo) string {
		name := info.Name()
		if info.Planner {
			name = "✦ planner"
		}
		out := " " + st.text.Render(name)
		if info.Kind != "" {
			out += st.faint.Render(" · " + info.Kind)
		}
		if !info.Planner && m.removed(info.Path) {
			out += st.faint.Render(" (removed)")
		}
		return out
	}
	if l != nil {
		as := l.Agents()
		slices.SortStableFunc(as, func(a, b usage.AgentSum) int {
			return cmp.Compare(b.Sum.Tokens.Total(), a.Sum.Tokens.Total())
		})
		for _, a := range as {
			uid := a.ID
			inLedger[uid] = true
			agents = append(agents, row(label(a.AgentInfo), a.Sum, func(s *session) bool { return s.uid == uid }))
		}
	}
	for _, s := range m.allSessions() {
		if s.running && s.turnID != "" && !inLedger[s.uid] {
			uid := s.uid
			agents = append(agents, row(label(usage.AgentInfo{ID: uid, Kind: string(s.kind), Branch: m.branchOf(s), Path: s.path, Planner: s.planner}),
				usage.Sum{}, func(s *session) bool { return s.uid == uid }))
		}
	}
	if len(agents) > 0 {
		rows = append(rows, usageRow{}, usageRow{label: st.muted.Render("Top agents"), heading: true})
		rows = append(rows, agents...)
	}
	return rows
}

// removed reports whether the worktree at path is gone: not in the latest
// scan and without an agent.
func (m *Model) removed(path string) bool {
	if path == "" || m.snap == nil {
		return false
	}
	return m.snapIndex(path) < 0 && m.sessionFor(path) == nil
}

// usagePane shows the token usage table in place of both panes.
func (m *Model) usagePane(w, h int) string {
	st := m.st
	inner := w - 2
	ih := h - 2
	cols := m.usageCols(inner - 1)
	labelW := inner - 1
	for _, c := range cols {
		labelW -= c.width + 1
	}
	line := func(r usageRow) string {
		out := " " + fitCell(r.label, labelW, 'l')
		if r.label == "" || r.heading {
			return out
		}
		for _, c := range cols {
			out += " " + fitCell(c.cell(r), c.width, 'r')
		}
		return out
	}
	header := " " + strings.Repeat(" ", labelW)
	for _, c := range cols {
		header += " " + fitCell(st.faint.Render(c.title), c.width, 'r')
	}

	var body []string
	if m.usage == nil {
		why := "grove has nowhere to keep it: this repository has no git folder"
		if m.usageErr != nil {
			why = m.usageErr.Error()
		}
		body = append(body, "", " "+st.muted.Render("Token usage isn't being recorded: ")+st.text.Render(why))
		return m.panel("Token usage", "", "", body, w, h, true)
	}
	rows := m.usageRows()
	legend := []string{"", " " + st.accent.Render("+ live") + st.faint.Render(" turns still running, not counted yet · ") +
		st.text.Render("≥") + st.faint.Render(" includes turns cut short · ") +
		st.text.Render("n/a") + st.faint.Render(" not reported · ") +
		st.text.Render("—") + st.faint.Render(" no cache figure")}
	if len(rows) <= 3 && rows[0].sum.Turns == 0 && !rows[0].running {
		legend = append([]string{"", " " + st.muted.Render("No agent has finished a turn yet.")}, legend...)
	}
	var lines []string
	for _, r := range rows {
		lines = append(lines, line(r))
	}
	lines = append(lines, legend...)

	fixed := []string{header}
	room := max(ih-len(fixed), 1)
	m.usageTop = min(m.usageTop, max(len(lines)-room, 0))
	end := min(m.usageTop+room, len(lines))
	body = append(fixed, lines[m.usageTop:end]...)
	foot := ""
	if len(lines) > room {
		foot = fmt.Sprintf("%d–%d of %d", m.usageTop+1, end, len(lines))
	}
	info := st.muted.Render("since grove started counting")
	return m.panel("Token usage", info, foot, body, w, h, true)
}
