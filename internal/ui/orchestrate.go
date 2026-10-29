package ui

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/charmbracelet/x/ansi"
)

// The planner can watch a running plan for the user, as its orchestrator.
// Who watches is asked before planning (watchAsk, unless [planner] watcher
// in the config answers it) and kept on the plan (plan.Orchestrated). For a
// watched plan grove sends the planner what would otherwise wait for the
// user: commands a task's agent was refused, a turn that ended with a
// question, and group chat messages. The planner answers with a
// grove-orchestrate block; grove carries out what it decided and leaves the
// rest for the user. At most cfg.Planner.Decisions() such turns run in a
// row before the user says something, so the planner and the agents can't
// keep each other going forever.

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
)

// orchItem is one thing waiting for the planner.
type orchItem struct {
	kind   orchKind
	plan   string   // plan ID
	branch string   // the task's, for approvals, questions, finished and failed tasks
	group  string   // group ID, for group messages
	lines  []string // the commands, the question, or the messages
	ids    []string // the approvals', alongside lines
	urgent bool     // starts a planner turn; other items wait for one
}

// orchestrator is the planner's state as the watcher of plans.
type orchestrator struct {
	queue   []orchItem
	turns   int             // planner turns in a row without the user
	paused  bool            // stopped at the limit until the user says something
	seen    map[string]int  // group chat messages already passed on, by folder
	asked   []string        // approvals in the planner's current turn, by ID
	reports []string        // its decisions that didn't apply, for its next request
	orders  map[string]bool // control orders already carried out, by ID
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
		{"The planner", "approves safe commands, answers what it can and follows the group chats; the rest waits for you"},
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

// watchedTask finds a task of a watched plan by branch, and its agent.
func (m *Model) watchedTask(branch string) (*plan.Plan, *plan.Task, *session) {
	for _, p := range m.plans {
		if !watched(p) {
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

// userActed lets the planner go on deciding after the user said something.
func (m *Model) userActed() {
	m.orch.turns, m.orch.paused = 0, false
}

// queueTurn hands a watched task's refused commands, or its closing
// question, to the planner.
func (m *Model) queueTurn(s *session) {
	p, t := m.taskFor(s.path)
	if t == nil || !watched(p) {
		return
	}
	var cmds, ids []string
	for _, a := range s.pending {
		if m.awaitsPlanner(s, a) {
			cmds, ids = append(cmds, a.text), append(ids, a.id)
		}
	}
	reply := strings.TrimSpace(s.lastReply.String())
	switch {
	case len(cmds) > 0:
		m.orch.queue = append(m.orch.queue, orchItem{kind: orchApproval, plan: p.ID, branch: t.Branch, lines: cmds, ids: ids, urgent: true})
	case slices.ContainsFunc(s.pending, func(a pendingAction) bool { return a.kind == pendingApproval }):
		// Only the user can answer what it waits on.
	case t.Status == plan.Working && !s.failed && endsWithQuestion(reply):
		if r := []rune(reply); len(r) > 2000 {
			reply = "…" + string(r[len(r)-2000:])
		}
		m.orch.queue = append(m.orch.queue, orchItem{kind: orchQuestion, plan: p.ID, branch: t.Branch, lines: []string{reply}, urgent: true})
	case t.Status == plan.Failed:
		m.orch.queue = append(m.orch.queue, orchItem{kind: orchFailed, plan: p.ID, branch: t.Branch, lines: []string{t.Error}, urgent: true})
	case t.Status == plan.Finished:
		// The planner checks the work before the user merges it.
		m.orch.queue = append(m.orch.queue, orchItem{kind: orchFinished, plan: p.ID, branch: t.Branch, lines: []string{tail(reply, 2000)}, urgent: true})
	}
}

// queueGroups passes new messages in watched plans' chats to the planner.
// Messages to everyone start a turn, and in the plan chat so does anything
// the user says; messages between two agents wait for the next one. What
// was there before grove started isn't news.
func (m *Model) queueGroups() {
	if m.orch.seen == nil {
		m.orch.seen = map[string]int{}
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
			if !known || len(g.msgs) <= seen {
				continue
			}
			it := orchItem{kind: orchGroup, plan: p.ID, group: grp.ID}
			for _, msg := range g.msgs[seen:] {
				// The planner hears about merges and tests directly.
				if msg.From == group.Planner || grp.ID == plan.ChatID && msg.From == group.Grove {
					continue
				}
				to := ""
				if msg.To != "" {
					to = " → " + msg.To
				}
				it.lines = append(it.lines, msg.From+to+": "+msg.Text)
				switch {
				case grp.ID == plan.ChatID && msg.From == group.User:
					it.urgent = true
				default:
					it.urgent = it.urgent || msg.To == "" && msg.From != group.User
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
		case orchGroup, orchMerge:
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
			if it.kind == orchQuestion && t.Status != plan.Working ||
				it.kind == orchFinished && t.Status != plan.Finished ||
				it.kind == orchFailed && t.Status != plan.Failed {
				continue
			}
		}
		out = append(out, it)
	}
	return out
}

// orchestrate starts a planner turn for what's queued, once the planner is
// free and something needs an answer.
func (m *Model) orchestrate() tea.Cmd {
	o := &m.orch
	o.queue = m.liveItems(o.queue)
	if !slices.ContainsFunc(o.queue, func(it orchItem) bool { return it.urgent }) {
		return nil
	}
	if m.planner != nil && m.planner.running {
		return nil
	}
	if o.turns >= m.cfg.Planner.Decisions() {
		if o.paused {
			return nil
		}
		o.paused = true
		return tea.Batch(
			m.notify(fmt.Sprintf("✦ The planner paused after %d decisions in a row; say something to it (P) or an agent to let it go on", o.turns), toastInfo),
			m.alert(fmt.Sprintf("The planner paused after %d decisions in a row and waits for you", o.turns)))
	}
	if m.planner == nil {
		dir := m.plannerDir()
		k, ok := agent.Parse(m.planByID(o.queue[0].plan).Agent)
		if dir == "" || !ok {
			return nil
		}
		m.planner = m.newPlanner(dir, k)
	}
	o.turns++
	text, summary := m.orchPrompt(o.queue)
	for _, it := range o.queue {
		o.asked = append(o.asked, it.ids...)
	}
	s := m.planner
	s.nextPlan = planIDs(o.queue)[0] // before the queue is cleared
	o.queue, o.reports = nil, nil
	cmd := m.send(s, text)
	// The chat shows a line about it, not the whole request.
	s.entries[len(s.entries)-1] = chatEntry{roleNote, summary}
	return cmd
}

// orchPrompt is the planner's request to decide on items, and a short
// account of it for its chat.
func (m *Model) orchPrompt(items []orchItem) (string, string) {
	var b strings.Builder
	var what []string
	b.WriteString("[grove] You are watching these plans' agents for the user. Decide what you can; whatever you leave out waits for the user.\n")
	for _, id := range planIDs(items) {
		p := m.planByID(id)
		fmt.Fprintf(&b, "\n## Plan %q (id %s)\n", p.Title, p.ID)
		for _, it := range items {
			if it.plan != id {
				continue
			}
			switch it.kind {
			case orchApproval:
				_, t, _ := m.watchedTask(it.branch)
				fmt.Fprintf(&b, "\n%s (task %q) wants to run commands it isn't allowed to:\n", it.branch, t.Title)
				for i, c := range it.lines {
					fmt.Fprintf(&b, "- id %s: `%s`\n", it.ids[i], c)
				}
				what = append(what, fmt.Sprintf("%s wants to run %s", it.branch, plural(len(it.lines), "command")))
			case orchQuestion:
				_, t, _ := m.watchedTask(it.branch)
				fmt.Fprintf(&b, "\n%s (task %q) finished its turn with this, and is waiting for an answer:\n\n%s\n", it.branch, t.Title, quoteText(it.lines[0]))
				what = append(what, it.branch+" asked a question")
			case orchFinished:
				_, t, _ := m.watchedTask(it.branch)
				fmt.Fprintf(&b, "\n%s (task %q) says it is done:\n\n%s\n\nCheck its work, such as its diff and tests, before the user merges it. "+
					"If something is missing, reply to tell it what; if it can't be done as planned, mark it failed.\n", it.branch, t.Title, quoteText(it.lines[0]))
				what = append(what, it.branch+" finished")
			case orchFailed:
				_, t, _ := m.watchedTask(it.branch)
				fmt.Fprintf(&b, "\n%s (task %q) failed: %s\nReply to it to try again another way, or leave it for the user.\n", it.branch, t.Title, it.lines[0])
				what = append(what, it.branch+" failed")
			case orchSettled:
				fmt.Fprintf(&b, "\nEvery task of this plan is finished or failed. Once you have checked the finished work and it is right, merge the plan "+
					"(the merge tool, or \"merge\" in the block): grove commits each finished task's worktree and merges their branches into the plan's own worktree for the user to try.\n")
				what = append(what, p.Title+" is ready to merge")
			case orchMerge:
				fmt.Fprintf(&b, "\n%s\n", it.lines[0])
				what = append(what, "merging "+p.Title)
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
	if len(m.orch.reports) > 0 {
		b.WriteString("\nSome of your earlier decisions didn't apply:\n")
		for _, r := range m.orch.reports {
			b.WriteString("- " + r + "\n")
		}
	}
	b.WriteString("\nAnswer with one fenced code block whose language is grove-orchestrate, containing JSON; leave out the lists you don't need. " +
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
	if len(m.controlTools(m.planner)) > 0 {
		b.WriteString("You can also act during your turn with grove's tools (tasks, message, approve, deny, mark, post, merge, close) instead of the block, " +
			"and you may run shell commands without asking, for example to read a task's diff or run its tests in its worktree. " +
			"Don't change any files yourself; the task agents do the work.\n\n")
	}
	b.WriteString("Approve a command only when it is safe and needed for the task: builds, tests, linters, formatters, installing what the task calls for. " +
		"Deny anything destructive, anything that reaches outside the worktree, and pushing or publishing. When unsure, leave it out so the user decides. " +
		"Commands grove's policy keeps for the user (protected branches, anything destructive) aren't listed and can't be approved by you. " +
		"grove reports every decision that doesn't apply. " +
		"Reply to a question only when the plan and the code answer it; leave questions about what the user wants for the user. " +
		"Post in a group chat only to settle something the agents are stuck on or disagree about. " +
		"Answer the user in the plan chat (group \"plan\"), not in your reply: they read the plan chat. Keep messages short.\n")
	return b.String(), "Asked to decide: " + strings.Join(what, "; ")
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
func (m *Model) applyDecisions(reply string) tea.Cmd {
	found := orchBlock.FindAllStringSubmatch(reply, -1)
	if len(found) == 0 {
		return nil
	}
	var d decisions
	if err := json.Unmarshal([]byte(found[len(found)-1][1]), &d); err != nil {
		m.plannerNote(roleError, "Couldn't read the planner's decisions: "+err.Error())
		m.orch.reports = append(m.orch.reports, "your last grove-orchestrate block wasn't valid JSON, so none of it applied: "+err.Error())
		return nil
	}
	cmd, outs := m.decide(d)
	for _, o := range outs {
		switch o.result {
		case control.Applied:
		case control.AlreadyApplied:
			m.plannerNote(roleNote, "Already done: "+o.String())
		default:
			m.plannerNote(roleNote, "Not applied: "+o.String())
			m.orch.reports = append(m.orch.reports, o.String())
		}
	}
	return cmd
}

// decide carries out the planner's decisions on watched plans' tasks and
// says what came of each.
func (m *Model) decide(d decisions) (tea.Cmd, []outcome) {
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
			cmds = append(cmds, m.answer(s, i, approve, byPlanner, a.Reason))
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
		switch s := a.s; {
		case a.busy:
			outs[a.out].detail = "the agent gets it when its turn ends"
		case s.running:
			outs[a.out].detail = "the agent was resumed with it"
		default:
			outs[a.out].detail = "the agent carries on once you answer the rest of what it waits on"
		}
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
			cmds = append(cmds, m.tell(s, "From the planner, who is watching this plan for the user: "+strings.TrimSpace(r.Message)))
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
			m.plannerNote(roleNote, text+".")
			cmds = append(cmds, m.finishIfSettled(p), m.savePlan(p), m.schedule())
		}
		outs = append(outs, o)
	}
	for _, p := range d.Post {
		o := outcome{order: p.order, what: "post in " + cmp.Or(p.Group, plan.ChatID), result: control.Applied}
		if cmd := m.plannerPost(p.Plan, p.Group, p.To, p.Message); cmd != nil {
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
	if s.running {
		s.notes = append(s.notes, text)
		return nil
	}
	return m.send(s, text)
}

// plannerPost posts the planner's message in a watched plan's chat. Every
// plan has a plan chat, so without planID it goes to the newest one still
// going.
func (m *Model) plannerPost(planID, groupID, to, text string) tea.Cmd {
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
		if !slices.ContainsFunc(p.ChatMembers(groupID), func(t *plan.Task) bool { return t.Branch == to }) {
			to = ""
		}
		msg := group.Message{From: group.Planner, To: to, Text: text}
		return func() tea.Msg {
			if err := log.Post(msg); err != nil {
				return actionMsg{err: fmt.Errorf("couldn't post the planner's message: %w", err)}
			}
			return groupPostedMsg{}
		}
	}
	return nil
}
