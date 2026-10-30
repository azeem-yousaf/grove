package ui

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/usage"
	"github.com/charmbracelet/x/ansi"
)

// The planner can watch a running plan for the user, as its orchestrator:
// it is then in charge, and the user only reviews the pull request at the
// end.
// Who watches is asked before planning (watchAsk, unless [planner] watcher
// in the config answers it) and kept on the plan (plan.Orchestrated). For a
// watched plan grove sends the planner what would otherwise wait for the
// user: commands a task's agent was refused, a turn that ended with a
// question, and group chat messages. The planner answers with a
// grove-orchestrate block; grove carries out what it decided, asks again
// about what it left out, and denies approvals it keeps leaving. At most
// cfg.Planner.Decisions() such turns run in a row without the plan moving
// forward or the user saying something, so the planner and the agents
// can't keep each other going forever.

// watchAsk asks who will watch the plan about to be made.
type watchAsk struct {
	cursor   int // 0 the user, 1 the planner
	remember bool
	then     func() tea.Cmd
}

type orchKind uint8

const (
	orchApproval orchKind = iota // commands a task's agent was refused
	orchQuestion                 // a task's turn ended with a question
	orchGroup                    // new group chat messages
	orchFinished                 // a task's agent says it is done
	orchFailed                   // a task failed
	orchSettled                  // every task of a plan is finished or failed
	orchMerge                    // how merging a plan went
	orchReview                   // new reviews and comments on a plan's pull request
	orchIdle                     // a task's turn ended without finishing, asking or waiting for the planner
)

// idleWait is how long an idle task waits for another reason to start a
// planner turn before it starts one itself.
const idleWait = 2 * time.Minute

// orchItem is one thing waiting for the planner.
type orchItem struct {
	kind   orchKind
	plan   string    // plan ID
	branch string    // the task's, for approvals, questions, finished, failed and idle tasks
	group  string    // group ID, for group messages
	lines  []string  // the commands, the question, or the messages
	ids    []string  // the approvals', alongside lines
	urgent bool      // starts a planner turn; other items wait for one
	now    bool      // starts it without waiting for more items (the user is involved)
	idle   time.Time // when an idle task was queued, to make it urgent after idleWait

	// When orchestrate first saw it, for waiting until items settle.
	at time.Time

	// Group messages: the chat's folder and how many of its messages the
	// item covers, so the planner's read position (orchestrator.told)
	// moves once it is sent.
	dir  string
	upto int
	// A finished task's HEAD, which becomes its session's finishedHead once
	// the planner is asked to check it.
	head string
}

// orchestrator is grove's state for the planners watching plans: what
// waits for them, and the lead planner's own state (each group's planner
// keeps its own in its session).
type orchestrator struct {
	plannerState
	queue  []orchItem
	seen   map[string]int  // group chat messages already queued, by folder
	told   map[string]int  // ... and sent to a planner (or history), saved in orchFile
	orders map[string]bool // control orders already carried out, by ID
}

// askWatcher runs then once it is known who watches the next plan: from the
// config, an earlier answer, or by asking.
func (m *Model) askWatcher(then func() tea.Cmd) tea.Cmd {
	if m.planWatch != "" {
		return then()
	}
	switch w := m.cfg.Planner.Watcher; w {
	case config.WatchYou, config.WatchPlanner:
		m.planWatch = w
		return then()
	}
	m.watchAsk = &watchAsk{then: then}
	return nil
}

func (m *Model) onWatchKey(msg tea.KeyPressMsg) tea.Cmd {
	a := m.watchAsk
	switch msg.String() {
	case "ctrl+c":
		return m.quitNow()
	case "esc", "q":
		m.watchAsk = nil
	case "up", "k":
		a.cursor = 0
	case "down", "j":
		a.cursor = 1
	case "space", "r":
		a.remember = !a.remember
	case "1", "2":
		a.cursor = int(msg.String()[0] - '1')
		return m.chooseWatcher()
	case "enter":
		return m.chooseWatcher()
	}
	return nil
}

func (m *Model) chooseWatcher() tea.Cmd {
	a := m.watchAsk
	m.watchAsk = nil
	w := config.WatchYou
	if a.cursor == 1 {
		w = config.WatchPlanner
	}
	m.planWatch = w
	cmds := []tea.Cmd{a.then()}
	if a.remember {
		m.cfg.Planner.Watcher = w
		save := m.saveWatch
		cmds = append(cmds, func() tea.Msg { return agentSavedMsg{save(w)} })
	}
	return tea.Batch(cmds...)
}

// watchModal asks who watches the plan.
func (m *Model) watchModal() string {
	a, st := m.watchAsk, m.st
	opts := []struct{ name, what string }{
		{"Me", "everything waits for you"},
		{"The planner", "takes charge: approves commands (all but destructive git), answers questions, helps stuck agents, merges, tests and opens a pull request for you to review"},
	}
	width := max(min(m.width-12, 64), 20)
	parts := []string{
		st.bold.Foreground(st.p.accent).Render("Who watches this plan's agents?"),
		"",
		st.muted.Render(wrapText("While the plan runs, its agents ask to run commands, finish turns with questions and talk in group chats.", width)),
		"",
	}
	for i, o := range opts {
		mark, s := "  ", st.text
		if i == a.cursor {
			mark, s = st.accent.Render("▸ "), st.accent.Bold(true)
		}
		parts = append(parts, mark+s.Render(fmt.Sprintf("%d  %s", i+1, o.name)))
		for _, l := range strings.Split(wrapText(o.what, width-5), "\n") {
			parts = append(parts, "     "+st.faint.Render(l))
		}
	}
	box := st.faint.Render("[ ]")
	if a.remember {
		box = st.accent.Bold(true).Render("[x]")
	}
	parts = append(parts, "", box+" "+st.text.Render("Remember my choice")+st.faint.Render("  (o in the plan review changes it)"), "",
		m.hints([][2]string{{"enter", "choose"}, {"1/2", "pick"}, {"space", "remember"}, {"esc", "cancel"}}))
	return st.modal.Render(strings.Join(parts, "\n"))
}

// wrapText breaks s into lines of at most width cells.
func wrapText(s string, width int) string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(s) {
		if line != "" && ansi.StringWidth(line)+1+ansi.StringWidth(w) > width {
			lines = append(lines, line)
			line = ""
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	return strings.Join(append(lines, line), "\n")
}

// watched reports whether the planner watches p.
func watched(p *plan.Plan) bool { return p != nil && p.Orchestrated && p.Status != plan.Draft }

// watchedBy reports whether the planner watches the task in path.
func (m *Model) watchedBy(path string) bool {
	p, t := m.taskFor(path)
	return t != nil && watched(p)
}

// watchedTask finds a task of a watched plan that isn't done by branch,
// and its agent. A closed plan's branch name may be reused by a later one.
func (m *Model) watchedTask(branch string) (*plan.Plan, *plan.Task, *session) {
	for _, p := range m.plans {
		if !watchedLive(p) {
			continue
		}
		for i := range p.Tasks {
			if t := &p.Tasks[i]; t.Branch == branch && t.Path != "" {
				return p, t, m.sessionFor(t.Path)
			}
		}
	}
	return nil, nil, nil
}

// userActed lets the planners go on deciding after the user said
// something.
func (m *Model) userActed() {
	m.progressed()
}

// progressed lets the planner go on deciding after a watched plan moved
// forward (a task finished, the plan merged, its tests passed), so the
// limit only stops a planner and agents that go round in circles.
func (m *Model) progressed() {
	m.orch.turns, m.orch.paused = 0, false
	for _, s := range m.crews {
		s.crew.turns, s.crew.paused = 0, false
	}
}

// queueTurn hands a watched task's refused commands, its closing question,
// or the news that it finished, failed or stopped to the planner. again is
// set when the task was already finished before the turn: the planner then
// hears about it only if its HEAD moved since (checked by a command).
func (m *Model) queueTurn(s *session, again bool) tea.Cmd {
	p, t := m.taskFor(s.path)
	if t == nil || !watched(p) {
		return nil
	}
	var cmds, ids []string
	for i := range s.pending {
		a := &s.pending[i]
		if a.id == "" {
			a.id = s.nextApprovalID() // a command card saved by an older grove
		}
		if m.awaitsPlanner(s, *a) {
			cmds, ids = append(cmds, a.text), append(ids, a.id)
		}
	}
	reply := strings.TrimSpace(s.lastReply.String())
	switch {
	case len(cmds) > 0:
		userToo := slices.ContainsFunc(s.pending, func(a pendingAction) bool { return a.policy.userOnly })
		m.orch.queue = append(m.orch.queue, orchItem{kind: orchApproval, plan: p.ID, branch: t.Branch, lines: cmds, ids: ids, urgent: true, now: userToo})
	case slices.ContainsFunc(s.pending, func(a pendingAction) bool { return a.kind == pendingApproval }):
		// Only the user can answer what it waits on.
	case t.Status == plan.Working && !s.failed && looksLikeQuestion(reply):
		if r := []rune(reply); len(r) > 2000 {
			reply = "…" + string(r[len(r)-2000:])
		}
		m.orch.queue = append(m.orch.queue, orchItem{kind: orchQuestion, plan: p.ID, branch: t.Branch, lines: []string{reply}, urgent: true})
	case t.Status == plan.Failed:
		m.orch.queue = append(m.orch.queue, orchItem{kind: orchFailed, plan: p.ID, branch: t.Branch, lines: []string{t.Error}, urgent: true})
	case t.Status == plan.Finished:
		// The planner checks the work before the user merges it.
		return m.checkFinished(s, p.ID, t.Branch, tail(reply, 2000), again)
	case t.Status == plan.Working && !m.gitBusy(s):
		// Neither done nor asking: the planner looks at it with its next
		// turn, or after idleWait.
		if !slices.ContainsFunc(m.orch.queue, func(it orchItem) bool { return it.kind == orchIdle && it.branch == t.Branch }) {
			m.orch.queue = append(m.orch.queue, orchItem{kind: orchIdle, plan: p.ID, branch: t.Branch, lines: []string{tail(reply, 2000)}, idle: m.now})
		}
	}
	return nil
}

// finishedHeadMsg is the HEAD of a task that says it is finished.
type finishedHeadMsg struct {
	path, plan, branch, reply string
	head                      string // "" when it couldn't be read
	again                     bool
}

// checkFinished reads the HEAD of a task that says it is finished, so a
// task that was already finished and committed nothing since doesn't make
// the planner check the same work again.
func (m *Model) checkFinished(s *session, planID, branch, reply string, again bool) tea.Cmd {
	ctx, path := m.ctx, s.path
	return func() tea.Msg {
		return finishedHeadMsg{path: path, plan: planID, branch: branch, reply: reply, head: git.RefHash(ctx, path, "HEAD"), again: again}
	}
}

func (m *Model) onFinishedHead(msg finishedHeadMsg) tea.Cmd {
	s := m.sessionFor(msg.path)
	p, t := m.taskFor(msg.path)
	if s == nil || t == nil || !watched(p) || t.Status != plan.Finished {
		return nil
	}
	if msg.again && msg.head != "" && msg.head == s.finishedHead {
		return nil
	}
	// finishedHead is set once the planner is asked (markSent), so news
	// still queued when grove closes is found again after a restart.
	it := orchItem{kind: orchFinished, plan: msg.plan, branch: msg.branch, lines: []string{msg.reply}, urgent: true, head: msg.head}
	if i := slices.IndexFunc(m.orch.queue, func(q orchItem) bool { return q.kind == orchFinished && q.branch == msg.branch }); i >= 0 {
		it.at = m.orch.queue[i].at
		m.orch.queue[i] = it
	} else {
		m.orch.queue = append(m.orch.queue, it)
	}
	return m.orchestrate()
}

// idleTick makes idle tasks urgent once they have waited idleWait.
func (m *Model) idleTick() tea.Cmd {
	woke := false
	for i := range m.orch.queue {
		if it := &m.orch.queue[i]; it.kind == orchIdle && !it.urgent && m.now.Sub(it.idle) >= idleWait {
			it.urgent, woke = true, true
		}
	}
	if !woke {
		return nil
	}
	return m.orchestrate()
}

// queueGroups passes new messages in watched plans' chats to the planner.
// Messages to everyone start a turn, and in the plan chat so does anything
// the user says; messages between two agents wait for the next one. What
// was there before grove started isn't news.
func (m *Model) queueGroups() {
	if m.orch.seen == nil {
		m.orch.seen = map[string]int{}
	}
	if m.orch.told == nil {
		m.orch.told = map[string]int{}
	}
	for _, p := range m.plans {
		if !watched(p) {
			continue
		}
		for _, grp := range p.Chats() {
			log, ok := m.groupLog(p, grp.ID)
			g := m.groups[log.Dir]
			if !ok || g == nil {
				continue
			}
			seen, known := m.orch.seen[log.Dir]
			m.orch.seen[log.Dir] = len(g.msgs)
			if !known {
				// A chat never read before: its history counts as told.
				m.orch.told[log.Dir] = len(g.msgs)
			}
			if !known || len(g.msgs) <= seen {
				continue
			}
			it := orchItem{kind: orchGroup, plan: p.ID, group: grp.ID, dir: log.Dir, upto: len(g.msgs)}
			owner := group.PlannerOf(chatOwner(grp.ID))
			for _, msg := range g.msgs[seen:] {
				// The owner doesn't hear its own posts, and the lead hears
				// about merges and tests directly.
				if msg.From == owner || grp.ID == plan.ChatID && msg.From == group.Grove {
					continue
				}
				to := ""
				if msg.To != "" {
					to = " → " + msg.To
				}
				it.lines = append(it.lines, msg.From+to+": "+msg.Text)
				switch {
				case msg.From == group.User:
					// The user talks to the planner in charge of that chat,
					// and waits for its answer.
					it.urgent, it.now = true, true
				case group.IsPlanner(msg.From), group.IsPlanner(msg.To), msg.To == group.User:
					// Another planner, or an agent asking for a decision: the
					// planner in charge answers for the user.
					it.urgent = true
				default:
					it.urgent = it.urgent || msg.To == ""
				}
			}
			if len(it.lines) > 0 {
				m.orch.queue = append(m.orch.queue, it)
			}
		}
	}
}

// liveItems drops what was settled while it waited: the plan isn't watched
// any more, the user answered the commands, or the agent is busy again.
func (m *Model) liveItems(items []orchItem) []orchItem {
	var out []orchItem
	for _, it := range items {
		p := m.planByID(it.plan)
		if !watched(p) {
			continue
		}
		switch it.kind {
		case orchGroup, orchMerge, orchReview:
		case orchSettled:
			if p.Status != plan.Running || !p.Settled() || m.mergingPlan[p.ID] || p.Merge != nil && p.Merge.Fixing != "" {
				continue
			}
		default:
			_, t, s := m.watchedTask(it.branch)
			if t == nil || s == nil || s.running {
				continue
			}
			if it.kind == orchApproval {
				if slices.ContainsFunc(out, func(o orchItem) bool { return o.kind == orchApproval && o.branch == it.branch }) {
					continue // one item lists them all
				}
				it.lines, it.ids = nil, nil
				for _, a := range s.pending {
					if m.awaitsPlanner(s, a) {
						it.lines, it.ids = append(it.lines, a.text), append(it.ids, a.id)
					}
				}
				if len(it.lines) == 0 {
					continue
				}
			}
			if (it.kind == orchQuestion || it.kind == orchIdle) && t.Status != plan.Working ||
				it.kind == orchFinished && t.Status != plan.Finished ||
				it.kind == orchFailed && t.Status != plan.Failed {
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

// orchestrate starts a turn for each planner that has something urgent
// queued and is free: the lead for ungrouped tasks and plan-wide news,
// each group's planner for its group.
func (m *Model) orchestrate() tea.Cmd {
	o := &m.orch
	o.queue = m.liveItems(o.queue)
	for i := range o.queue {
		if o.queue[i].at.IsZero() {
			o.queue[i].at = m.now
		}
	}
	var owners []string // crewKey, "" for the lead, in queue order
	byOwner := map[string][]orchItem{}
	for _, it := range o.queue {
		k := crewKey(it.plan, m.ownerOf(it))
		if _, ok := byOwner[k]; !ok {
			owners = append(owners, k)
		}
		byOwner[k] = append(byOwner[k], it)
	}
	var cmds []tea.Cmd
	var rest []orchItem
	for _, k := range owners {
		items := byOwner[k]
		cmd, sent := m.orchestrateOne(items)
		if !sent {
			// Approvals and questions needn't wait for a busy owner (lane.go).
			var lane tea.Cmd
			lane, items = m.orchestrateLane(items)
			rest = append(rest, items...)
			cmds = append(cmds, lane)
		}
		cmds = append(cmds, cmd)
	}
	o.queue = rest
	return tea.Batch(cmds...)
}

// orchestrateOne starts a turn of the planner that owns items, all of one
// owner, reporting whether it took them.
func (m *Model) orchestrateOne(items []orchItem) (tea.Cmd, bool) {
	if !slices.ContainsFunc(items, func(it orchItem) bool { return it.urgent }) || !m.itemsSettled(items) {
		return nil, false
	}
	planID, groupID := items[0].plan, m.ownerOf(items[0])
	var cur *session
	if groupID == "" {
		cur = m.planner
	} else {
		cur = m.crews[crewKey(planID, groupID)]
	}
	if cur != nil && cur.running {
		return nil, false
	}
	st := m.stateOf(cur)
	if groupID != "" && cur == nil {
		st = &plannerState{}
	}
	if m.now.Before(st.retryAt) {
		return nil, false // its last turn failed: the clock tries again later
	}
	who := "The planner"
	if groupID != "" {
		who = "The planner of " + groupTitleOf(m.planByID(planID), groupID)
	}
	if st.turns >= m.cfg.Planner.Decisions() {
		if st.paused {
			return nil, false
		}
		st.paused = true
		return tea.Batch(
			m.notify(fmt.Sprintf("✦ %s paused after %d decisions in a row; say something to it (P) or an agent to let it go on", who, st.turns), toastInfo),
			m.alert(fmt.Sprintf("%s paused after %d decisions in a row and waits for you", who, st.turns))), false
	}
	s := m.plannerFor(planID, groupID)
	if s == nil {
		return nil, false
	}
	st = m.stateOf(s)
	st.turns++
	text, summary := m.orchPrompt(s, items)
	for _, it := range items {
		st.asked = append(st.asked, it.ids...)
	}
	s.nextPlan = planIDs(items)[0]
	s.nextKind = usage.KindDecide
	st.reports = nil
	st.sent = slices.Clone(items) // back in the queue if the turn fails
	m.markSent(items)
	rules, _ := m.rulesFor(s)
	cmd := m.send(s, text)
	s.rulesSent = rulesKey(rules)
	// The chat shows a line about it, not the whole request.
	s.entries[len(s.entries)-1] = chatEntry{roleNote, summary}
	return cmd, true
}

// itemsSettled reports whether a planner's turn for items may start: no
// item has come in for the planner's settle delay, so items arriving
// together go in one turn, or the oldest has waited the longest it may, or
// one involves the user.
func (m *Model) itemsSettled(items []orchItem) bool {
	delay := m.cfg.Planner.PlannerSettle()
	if delay <= 0 || slices.ContainsFunc(items, func(it orchItem) bool { return it.urgent && it.now }) {
		return true
	}
	var oldest, newest time.Time
	for _, it := range items {
		if !it.urgent {
			continue // waits for a turn anyway
		}
		if oldest.IsZero() || it.at.Before(oldest) {
			oldest = it.at
		}
		if it.at.After(newest) {
			newest = it.at
		}
	}
	return m.now.Sub(newest) >= delay || m.now.Sub(oldest) >= config.SettleMax
}

// groupTitleOf is a group's name, or its ID when it isn't known.
func groupTitleOf(p *plan.Plan, groupID string) string {
	if p != nil {
		if g, ok := p.Group(groupID); ok {
			return g.Name
		}
	}
	return groupID
}

// orchPrompt is the planner's request to decide on items, and a short
// account of it for its chat.
func (m *Model) orchPrompt(s *session, items []orchItem) (string, string) {
	var b strings.Builder
	var what []string
	if s.crewGroup != "" {
		b.WriteString("[grove] You are in charge of your group for the user. The user only reviews the pull request at the end, so decide everything here yourself; " +
			"nobody else will answer your group's agents.\n")
	} else {
		b.WriteString("[grove] You are in charge of these plans for the user. The user only reviews the pull request at the end, so decide everything here yourself; " +
			"nobody else will answer the agents. Each group has its own planner, which handles its tasks and chat.\n")
	}
	for _, id := range planIDs(items) {
		p := m.planByID(id)
		fmt.Fprintf(&b, "\n## Plan %q (id %s)\n", p.Title, p.ID)
		for _, it := range items {
			if it.plan != id {
				continue
			}
			switch it.kind {
			case orchApproval:
				_, t, ts := m.watchedTask(it.branch)
				fmt.Fprintf(&b, "\n%s (task %q) wants to run commands it isn't allowed to:\n", it.branch, t.Title)
				for i, c := range it.lines {
					fmt.Fprintf(&b, "- id %s: `%s`%s\n", it.ids[i], c, m.staleSuffix(ts, it.ids[i])+cardSuffix(ts, it.ids[i]))
				}
				what = append(what, fmt.Sprintf("%s wants to run %s", it.branch, plural(len(it.lines), "command")))
			case orchQuestion:
				_, t, _ := m.watchedTask(it.branch)
				fmt.Fprintf(&b, "\n%s (task %q) finished its turn with this, and is waiting for an answer:\n\n%s\n"+
					"Answer it yourself from the plan, the code and your judgment of what the user asked for.\n", it.branch, t.Title, quoteText(it.lines[0]))
				what = append(what, it.branch+" asked a question")
			case orchFinished:
				_, t, _ := m.watchedTask(it.branch)
				fmt.Fprintf(&b, "\n%s (task %q) says it is done:\n\n%s\n\nCheck its work, such as its diff and tests, before the user merges it. "+
					"If something is missing, reply to tell it what; if it can't be done as planned, mark it failed.\n", it.branch, t.Title, quoteText(it.lines[0]))
				what = append(what, it.branch+" finished")
			case orchIdle:
				_, t, _ := m.watchedTask(it.branch)
				fmt.Fprintf(&b, "\n%s (task %q) ended its turn without finishing or asking anything; it ended with:\n\n%s\n\n"+
					"Check whether it is done or stuck: read its chat's end in the tasks tool and its worktree. "+
					"Reply to it with what to do next, or mark it finished or failed.\n", it.branch, t.Title, quoteText(it.lines[0]))
				what = append(what, it.branch+" stopped")
			case orchFailed:
				_, t, _ := m.watchedTask(it.branch)
				fmt.Fprintf(&b, "\n%s (task %q) failed: %s\nFind out why: read its chat's end in the tasks tool, its worktree and its diff, and run what you need there. "+
					"Then fix what blocks it (run the commands it needs in its worktree, answer what it is unsure about) and reply to it with exactly what to do next, which starts it again. "+
					"Mark it failed only if the task can't be done at all; the plan then goes on without it.\n", it.branch, t.Title, it.lines[0])
				what = append(what, it.branch+" failed")
			case orchSettled:
				fmt.Fprintf(&b, "\nEvery task of this plan is finished or failed. Once you have checked the finished work and it is right, merge the plan "+
					"(the merge tool, or \"merge\" in the block): grove commits each finished task's worktree and merges their branches into the plan's own worktree for the user to try.\n")
				what = append(what, p.Title+" is ready to merge")
			case orchMerge:
				fmt.Fprintf(&b, "\n%s\n", it.lines[0])
				what = append(what, "merging "+p.Title)
			case orchReview:
				pr := ""
				if p.Merge != nil {
					pr = " " + p.Merge.PR
				}
				fmt.Fprintf(&b, "\nNew reviews and comments on the plan's pull request%s:\n", pr)
				for _, l := range it.lines {
					b.WriteString(quoteText(l) + "\n")
				}
				b.WriteString("Do what they ask: reply to the agents of the tasks that own that part (which reopens them) with exactly what to change, " +
					"and merge again once they are finished; grove tests it and pushes to the same pull request. " +
					"Answer questions, or say what you changed, on the pull request with gh pr comment. An approval alone needs nothing from you.\n")
				what = append(what, fmt.Sprintf("%s on %s's pull request", plural(len(it.lines), "review comment"), p.Title))
			case orchGroup:
				name := it.group
				if g, ok := p.Chat(it.group); ok {
					name = g.Name
				}
				if it.group == plan.ChatID {
					fmt.Fprintf(&b, "\nNew messages in the plan chat (group id %q), where the user talks to you:\n", plan.ChatID)
				} else {
					fmt.Fprintf(&b, "\nNew messages in the group chat %q (group id %q):\n", name, it.group)
				}
				for _, l := range it.lines {
					b.WriteString(quoteText(l) + "\n")
				}
				what = append(what, fmt.Sprintf("%s in %s", plural(len(it.lines), "message"), name))
			}
		}
	}
	if reports := m.stateOf(s).reports; len(reports) > 0 {
		b.WriteString("\nSome of your earlier decisions didn't apply:\n")
		for _, r := range reports {
			b.WriteString("- " + r + "\n")
		}
	}
	if rules, full := m.rulesFor(s); full {
		b.WriteString("\n" + rules)
	} else {
		b.WriteString("\nAnswer with a grove-orchestrate block as before (or grove's tools), following the rules you were given.\n")
	}
	return b.String(), "Asked to decide: " + strings.Join(what, "; ")
}

// rulesFor is the planner's standing instructions for deciding, and whether
// its request needs them: Claude keeps the conversation, so it gets them
// again only when they changed since it last did; Codex starts fresh every
// turn.
func (m *Model) rulesFor(s *session) (string, bool) {
	rules := m.orchRules(s)
	return rules, s.id == "" || s.kind == agent.Codex || s.rulesSent != rulesKey(rules)
}

// rulesKey fingerprints standing instructions, to tell whether a
// conversation already has them.
func rulesKey(rules string) string {
	sum := sha256.Sum256([]byte(rules))
	return hex.EncodeToString(sum[:8])
}

// orchRules is what doesn't change from one decision to the next: the
// block's format, the control tools and the rules for approvals.
func (m *Model) orchRules(s *session) string {
	var b strings.Builder
	b.WriteString("Answer with one fenced code block whose language is grove-orchestrate, containing JSON; leave out the lists you don't need. " +
		"Name commands by their id:\n\n")
	b.WriteString("```grove-orchestrate\n" + `{
  "approve": [{"branch": "feature/api", "id": "3fa2c1-1"}],
  "deny": [{"branch": "feature/api", "id": "3fa2c1-2", "reason": "what the agent should do instead"}],
  "reply": [{"branch": "feature/ui", "message": "your answer to its question"}],
  "post": [{"plan": "plan id", "group": "checkout", "to": "", "message": "to everyone, or set to a member's branch"}],
  "mark": [{"branch": "feature/docs", "status": "failed", "note": "why, for the user"}],
  "merge": [{"plan": "plan id"}],
  "close": [{"plan": "plan id", "note": "what the user accepted"}]
}` + "\n```\n\n")
	if len(m.controlTools(s)) > 0 {
		b.WriteString("You can also act during your turn with grove's tools (tasks, message, approve, deny, mark, post, merge, close) instead of the block, " +
			"and you may run shell commands without asking, for example to read a task's diff, run its tests, install its dependencies or run git grove allows in its worktree. " +
			"Don't change any files yourself; the task agents do the work.\n\n")
	}
	b.WriteString("Approve a command when it is safe and needed for the task: builds, tests, linters, formatters, installing what the task calls for; " +
		"git the task needs (committing, merging feature branches, pushing a task's own branch) is fine too. " +
		"Add \"prefix\": true to approve a routine check's whole family (every `go test`, not just this line; never git). " +
		"If one is marked as the worktree changed since it was asked, check the worktree before approving it: it may no longer do what the agent meant. " +
		"Deny anything destructive and anything that reaches outside the plan's worktrees, saying what to do instead. " +
		"Commands grove's policy keeps for the user (deleting branches or worktrees, force pushes, discarding work, protected branches) aren't listed and can't be approved by you. " +
		"Some are commands an agent asked grove to run for it (marked so): approving one runs it in the agent's worktree and gives the agent the output; denying dismisses it. " +
		"grove reports every decision that doesn't apply. " +
		"Answer every question and every approval you are given; anything you leave out is asked again, and an approval you keep leaving is denied. " +
		"When an agent is struggling (errors, failing builds or tests, missing tools or dependencies, going round in circles), take it on: look at its worktree, " +
		"run the commands it needs there yourself, and tell it exactly what to do next. Don't edit files yourself; the agents do. " +
		"Post in a group chat to settle anything the agents are stuck on or disagree about. " +
		"The lead planner posts as \"planner\" and each group's planner as \"planner:<group id>\"; planners hear each other's posts, " +
		"so a group's planner reaches the lead in the plan chat and the lead reaches a group's planner in that group's chat. " +
		"Answer the user in the plan chat (group \"plan\"), not in your reply: they read the plan chat. Keep messages short.\n")
	return b.String()
}

func planIDs(items []orchItem) []string {
	var out []string
	for _, it := range items {
		if !slices.Contains(out, it.plan) {
			out = append(out, it.plan)
		}
	}
	return out
}

func quoteText(s string) string {
	return "> " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n> ")
}

var orchBlock = regexp.MustCompile("(?s)```grove-orchestrate[ \\t]*\\r?\\n(.*?)```")

// decisions is the planner's grove-orchestrate block, or the orders its
// control tools queued.
type decisions struct {
	Approve []answerDecision
	Deny    []answerDecision
	Reply   []replyDecision
	Post    []postDecision
	Mark    []markDecision
	Merge   []planDecision
	Close   []planDecision
}

// Each decision may come from a control order, whose outcome grove records
// under the order's ID.
type (
	answerDecision struct {
		ID, Branch, Command, Reason string
		Prefix                      bool // approve the command's family, not just the line
		order                       string
	}
	replyDecision struct {
		Branch, Message string
		order           string
	}
	postDecision struct {
		Plan, Group, To, Message string
		order                    string
	}
	markDecision struct {
		Branch, Status, Note string
		order                string
	}
	planDecision struct {
		Plan, Note string
		order      string
	}
)

// outcome is what came of one decision.
type outcome struct {
	order  string // the control order's ID, "" for a block's decision
	what   string // the decision, for the planner
	result string // control.Applied and so on
	detail string
}

func (o outcome) String() string {
	s := o.what + ": " + o.result
	if o.detail != "" {
		s += " (" + o.detail + ")"
	}
	return s
}

// applyDecisions carries out the last grove-orchestrate block in a planner
// reply. Only tasks of watched plans are touched; what didn't apply is
// noted in the planner's chat and told in its next request.
func (m *Model) applyDecisions(s *session, reply string) tea.Cmd {
	found := orchBlock.FindAllStringSubmatch(reply, -1)
	if len(found) == 0 {
		return nil
	}
	st := m.stateOf(s)
	var d decisions
	if err := json.Unmarshal([]byte(found[len(found)-1][1]), &d); err != nil {
		noteTo(s, roleError, "Couldn't read the planner's decisions: "+err.Error())
		st.reports = append(st.reports, "your last grove-orchestrate block wasn't valid JSON, so none of it applied: "+err.Error())
		return nil
	}
	d, refused := m.scopeDecisions(s, d)
	cmd, outs := m.decide(s, d)
	for _, o := range append(refused, outs...) {
		switch o.result {
		case control.Applied:
		case control.AlreadyApplied:
			noteTo(s, roleNote, "Already done: "+o.String())
		default:
			noteTo(s, roleNote, "Not applied: "+o.String())
			st.reports = append(st.reports, o.String())
		}
	}
	return cmd
}

// decide carries out planner by's decisions (nil: the lead's control
// tools) on watched plans' tasks and says what came of each.
func (m *Model) decide(by *session, d decisions) (tea.Cmd, []outcome) {
	if by == nil {
		by = m.planner
	}
	var cmds []tea.Cmd
	var outs []outcome
	type applied struct {
		out  int
		s    *session
		busy bool // in a turn already
	}
	var answered []applied
	answer := func(a answerDecision, approve bool) {
		verb := "deny"
		if approve {
			verb = "approve"
		}
		name := a.ID
		if name == "" {
			name = "`" + a.Command + "`"
		}
		o := outcome{order: a.order, what: fmt.Sprintf("%s %s for %s", verb, name, a.Branch), result: control.Applied}
		if cs, ci := m.findCard(a.Branch, a.ID, a.Command); cs != nil {
			// A command the agent asked grove to run (cards.go).
			cmd, o2, resume := m.decideCard(cs, ci, approve, a.Reason, o)
			cmds, o = append(cmds, cmd), o2
			if resume {
				answered = append(answered, applied{len(outs), cs, cs.running})
			}
			outs = append(outs, o)
			return
		}
		s, i, rec := m.findApproval(a.Branch, a.ID, a.Command)
		switch {
		case s == nil:
			o.result, o.detail = control.NotFound, "nothing a watched task waits on has that id or command; check tasks"
		case rec != nil && rec.Approve == approve:
			o.result, o.detail = control.AlreadyApplied, "it was already "+map[bool]string{true: "approved", false: "denied"}[approve]+" by the "+rec.By
		case rec != nil:
			o.result, o.detail = control.Failed, "it was already "+map[bool]string{true: "approved", false: "denied"}[rec.Approve]+" by the "+rec.By
		case approve && s.pending[i].policy.userOnly:
			p := s.pending[i]
			o.result, o.detail = control.NeedsUser, p.policy.reason
			if o.detail == "" {
				o.detail = "only the user may approve it"
			}
			cmds = append(cmds, m.refuse(s, p))
		default:
			rule := ""
			if approve && a.Prefix {
				p := s.pending[i]
				if r, ok := m.familyRule(s, p.tool, p.text); ok {
					rule = r
				} else {
					o.detail = "approved the exact command only: prefix approval isn't for git, interpreters or more than one command"
				}
			}
			cmds = append(cmds, m.answerWith(s, i, approve, byPlanner, a.Reason, rule))
			answered = append(answered, applied{len(outs), s, s.running})
		}
		outs = append(outs, o)
	}
	for _, a := range d.Approve {
		answer(a, true)
	}
	for _, a := range d.Deny {
		answer(a, false)
	}
	for _, s := range uniqueSessions(answered, func(a applied) *session { return a.s }) {
		cmds = append(cmds, m.resume(s))
	}
	for _, a := range answered {
		detail := "the agent carries on once you answer the rest of what it waits on"
		switch s := a.s; {
		case a.busy:
			detail = "the agent gets it when its turn ends"
		case s.running:
			detail = "the agent was resumed with it"
		}
		if d := outs[a.out].detail; d != "" {
			detail = d + "; " + detail
		}
		outs[a.out].detail = detail
	}
	for _, r := range d.Reply {
		o := outcome{order: r.order, what: "reply to " + r.Branch, result: control.Applied, detail: "it reaches the agent as soon as it isn't working; you'll hear when its turn ends"}
		p, t, s := m.watchedTask(r.Branch)
		switch {
		case s == nil:
			o.result, o.detail = control.NotFound, "no watched task has that branch, or it hasn't started"
		case strings.TrimSpace(r.Message) == "":
			o.result, o.detail = control.Failed, "the message is empty"
		default:
			// More to do: a finished or failed task works again.
			if t.Status != plan.Working {
				t.Status, t.Error, p.Status = plan.Working, "", plan.Running
				cmds = append(cmds, m.savePlan(p))
			}
			cmds = append(cmds, m.tellAs(s, replyKind(p), "From the planner, who is watching this plan for the user: "+strings.TrimSpace(r.Message)))
		}
		outs = append(outs, o)
	}
	for _, mk := range d.Mark {
		o := outcome{order: mk.order, what: "mark " + mk.Branch + " " + mk.Status, result: control.Applied}
		p, t, _ := m.watchedTask(mk.Branch)
		switch {
		case t == nil:
			o.result, o.detail = control.NotFound, "no watched task has that branch"
		case mk.Status != "finished" && mk.Status != "failed":
			o.result, o.detail = control.Failed, `status must be "finished" or "failed"`
		default:
			t.Status, t.Error = plan.Finished, ""
			if mk.Status == "failed" {
				t.Status, t.Error = plan.Failed, strings.TrimSpace(mk.Note)
			}
			text := "The planner marked " + t.Title + " " + mk.Status
			if n := strings.TrimSpace(mk.Note); n != "" {
				text += ": " + n
			}
			noteTo(by, roleNote, text+".")
			cmds = append(cmds, m.finishIfSettled(p), m.savePlan(p), m.schedule())
		}
		outs = append(outs, o)
	}
	for _, p := range d.Post {
		o := outcome{order: p.order, what: "post in " + cmp.Or(p.Group, plan.ChatID), result: control.Applied}
		if cmd := m.plannerPost(plannerName(by), p.Plan, p.Group, p.To, p.Message); cmd != nil {
			cmds = append(cmds, cmd)
		} else {
			o.result, o.detail = control.NotFound, "no watched plan has that chat, or the message is empty"
		}
		outs = append(outs, o)
	}
	for _, mg := range d.Merge {
		o := outcome{order: mg.order, what: "merge " + mg.Plan, result: control.Applied, detail: "merging; you'll hear how it went"}
		if p := m.findPlan(mg.Plan); watched(p) {
			cmds = append(cmds, m.mergePlan(p))
		} else {
			o.result, o.detail = control.NotFound, "no watched plan has that ID or title"
		}
		outs = append(outs, o)
	}
	for _, c := range d.Close {
		o := outcome{order: c.order, what: "close " + c.Plan, result: control.Applied}
		if p := m.findPlan(c.Plan); watched(p) {
			cmds = append(cmds, m.closePlan(p, c.Note))
		} else {
			o.result, o.detail = control.NotFound, "no watched plan has that ID or title"
		}
		outs = append(outs, o)
	}
	return tea.Batch(cmds...), outs
}

// uniqueSessions is each session once, in order.
func uniqueSessions[T any](items []T, of func(T) *session) []*session {
	var out []*session
	for _, it := range items {
		if s := of(it); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// tell sends a task's agent a message from grove or the planner: now if
// it is free, otherwise with its next turn.
func (m *Model) tell(s *session, text string) tea.Cmd {
	return m.tellAs(s, usage.KindWork, text)
}

// tellAs is tell for a turn of kind k; a message that waits for the next
// turn leaves that turn's kind alone.
func (m *Model) tellAs(s *session, k usage.Kind, text string) tea.Cmd {
	if s.running {
		s.notes = append(s.notes, text)
		return nil
	}
	return m.sendAs(s, k, text)
}

// replyKind is the kind of a planner's reply to a task of p: fixing, after
// p's merged work failed its tests, else work.
func replyKind(p *plan.Plan) usage.Kind {
	if p.Merge != nil && p.Merge.Tests == plan.TestsFailed {
		return usage.KindFix
	}
	return usage.KindWork
}

// plannerPost posts a planner's message in a watched plan's chat, from
// group.Planner (the lead) or "planner:<group>" (plannerName). Every plan
// has a plan chat, so without planID it goes to the newest one still going.
func (m *Model) plannerPost(from, planID, groupID, to, text string) tea.Cmd {
	text = strings.TrimSpace(text)
	if groupID == "" {
		groupID = plan.ChatID
	}
	if text == "" {
		return nil
	}
	for i := len(m.plans) - 1; i >= 0; i-- {
		p := m.plans[i]
		if !watched(p) || planID != "" && p.ID != planID && p.Title != planID || planID == "" && p.Status == plan.Done {
			continue
		}
		if _, ok := p.Chat(groupID); !ok {
			continue
		}
		log, ok := m.groupLog(p, groupID)
		if !ok {
			return nil
		}
		if to != group.User && !group.IsPlanner(to) && !slices.ContainsFunc(p.ChatMembers(groupID), func(t *plan.Task) bool { return t.Branch == to }) {
			to = ""
		}
		if g := m.groups[log.Dir]; g != nil {
			g.wakeups, g.paused = 0, false
		}
		msg := group.Message{From: from, To: to, Text: text}
		return func() tea.Msg {
			if err := log.Post(msg); err != nil {
				return actionMsg{err: fmt.Errorf("couldn't post the planner's message: %w", err)}
			}
			return groupPostedMsg{}
		}
	}
	return nil
}
