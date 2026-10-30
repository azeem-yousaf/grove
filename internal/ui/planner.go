package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// The planner is an agent that reads the repository but never changes it.
// The user gives it requirements, typed or as Jira issues; it asks what it
// needs to and proposes a plan: tasks, each with a branch, a brief and
// optionally a group. The user reviews the plan (review.go) and starts it,
// which creates a worktree per task and starts its agent, a few at a time.

type (
	plansLoadedMsg struct {
		plans []*plan.Plan
		err   error
	}
	branchNamesMsg struct{ names []string }
	planCheckedMsg struct {
		id       string
		problems []string
		branches git.Branches
		err      error
	}
	taskCreatedMsg struct {
		plan, task string
		path       string
		branch     string
		copyErr    error
		err        error
	}
)

// plannerDir is where the planner runs: the main worktree, or failing
// that any worktree with files.
func (m *Model) plannerDir() string {
	if m.snap == nil {
		return ""
	}
	for _, s := range m.snap.States {
		if s.Main && !s.Bare && s.Err == nil && !s.Prunable {
			return s.Path
		}
	}
	for _, s := range m.snap.States {
		if !s.Bare && s.Err == nil && !s.Prunable {
			return s.Path
		}
	}
	return ""
}

func (m *Model) newPlanner(dir string, k agent.Kind) *session {
	return &session{kind: k, path: dir, name: "planner", model: m.roleModel(config.RoleLead, k), touched: map[string]bool{}, seen: true, planner: true, uid: newUID(), created: time.Now()}
}

// roleModel is the model for a plan's agent with role (config.RoleLead,
// RoleCrew or RoleTask) when it runs k.
func (m *Model) roleModel(role string, k agent.Kind) string {
	return m.cfg.Planner.ModelFor(role, string(k), m.cfg.AI)
}

// openPlanner shows the planner, starting one if needed. Before the next
// plan is made it asks who will watch that plan's agents.
func (m *Model) openPlanner() tea.Cmd {
	show := func() tea.Cmd { return tea.Batch(m.showChat(m.planner), m.loadBranchNames()) }
	if m.planner != nil {
		return m.askWatcher(show)
	}
	dir := m.plannerDir()
	if dir == "" {
		return m.notify("The planner needs a worktree with files to read", toastInfo)
	}
	return m.withAgent(func(k agent.Kind) tea.Cmd {
		m.planner = m.newPlanner(dir, k)
		return m.askWatcher(show)
	})
}

// loadBranchNames fetches the branches the planner must not propose again.
func (m *Model) loadBranchNames() tea.Cmd {
	repo := m.repo
	if repo.CommonDir == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		b, err := git.ListBranches(ctx, repo)
		if err != nil {
			return nil
		}
		return branchNamesMsg{takenBranches(b)}
	}
}

// takenBranches are names a new task branch can't use: local branches, and
// remote ones, which git would check out instead of making a new branch.
func takenBranches(b git.Branches) []string {
	var out []string
	for n := range b.Local {
		out = append(out, n)
	}
	for n := range b.Remote {
		if _, rest, ok := strings.Cut(n, "/"); ok && !b.Local[rest] {
			out = append(out, rest)
		}
	}
	slices.Sort(out)
	return out
}

// plannerContext briefs the planner on its job, the repository and the
// format its plan must take.
func (m *Model) plannerContext() string {
	s := m.planner
	var b strings.Builder
	b.WriteString("# You are grove's planner\n\n")
	b.WriteString("grove is a terminal app for git worktrees. The user gives you requirements, typed here or as Jira issues. " +
		"Your job is to understand them, read the code to see what is involved, and split the work into tasks that " +
		"separate AI agents will do at the same time, each in its own git worktree and branch.\n\n")
	b.WriteString("Rules:\n")
	fmt.Fprintf(&b, "- You can read the repository in %s but you never change anything: no edits, no git commands that change the repository.\n", s.path)
	b.WriteString("- When the user names a Jira issue or epic, read it (and its child issues) with the Atlassian tools. If those tools aren't available, say so and ask the user to paste the details.\n")
	b.WriteString("- Ask clarifying questions instead of guessing when the requirements are unclear or leave real choices open. Don't propose a plan until you understand what is wanted.\n")
	b.WriteString("- Each task must be doable on its own branch by one agent that sees only its brief and the code. Write each brief so that agent knows what to build, where in the code, any constraints, and how to tell it is done.\n")
	b.WriteString("- Put tasks in the same group only when their agents must coordinate while they work, for example an API and the UI that calls it. Grouped agents can message each other and read each other's worktrees; tasks outside a group are fully isolated. Prefer isolated tasks when no coordination is needed.\n")
	b.WriteString(plannerGitRules())
	b.WriteString("- Branch names are short and lowercase, like feature/guest-checkout, and must not already exist.\n")
	if m.base != "" {
		fmt.Fprintf(&b, "- New worktrees start from %s.\n", m.base)
	}
	b.WriteString("- Replies appear in a narrow terminal panel: keep prose short.\n")
	if m.snap != nil {
		b.WriteString("\n## Worktrees that already exist\n\n")
		for _, st := range m.snap.States {
			fmt.Fprintf(&b, "- %s at %s\n", st.Name(), st.Path)
		}
	}
	if len(m.branchNames) > 0 {
		names := m.branchNames
		if len(names) > 200 {
			names = names[:200]
		}
		b.WriteString("\n## Existing branches\n\n" + strings.Join(names, ", ") + "\n")
	}
	b.WriteString("\n## Writing the plan\n\nWhen you are ready, write the whole plan as one fenced code block whose language is grove-plan, containing JSON:\n\n")
	b.WriteString("```grove-plan\n" + `{
  "title": "Guest checkout",
  "test": "npm test",
  "groups": [{"id": "checkout", "name": "Checkout flow", "icon": "🛒"}],
  "tasks": [
    {"id": "api", "title": "Guest checkout API", "branch": "feature/guest-checkout-api", "group": "checkout", "brief": "..."},
    {"id": "ui", "title": "Guest checkout page", "branch": "feature/guest-checkout-ui", "group": "checkout", "brief": "..."},
    {"id": "docs", "title": "Document guest checkout", "branch": "docs/guest-checkout", "brief": "..."}
  ]
}` + "\n```\n\n")
	b.WriteString("\"test\" is the command that builds and tests the whole repository once the tasks are merged, run from its root, " +
		"as the repository's own scripts or CI run it; leave it out if you can't tell. ")
	b.WriteString("Leave \"group\" out for isolated tasks. A group's \"icon\" is optional: one emoji shown next to its name. Outside the block, say in a sentence or two how you split the work. " +
		"The user reviews and edits the plan in grove before anything is created; if they ask for changes, write the whole plan again.\n")
	return b.String()
}

// plannerRequest is everything the user has asked the planner, which is
// what a plan proposed now is for.
func plannerRequest(s *session) string {
	var parts []string
	for _, e := range s.entries {
		if e.role == roleUser {
			parts = append(parts, e.text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// onPlannerReply turns a plan in the planner's reply into a draft, which
// replaces any earlier draft.
func (m *Model) onPlannerReply(s *session, reply string) tea.Cmd {
	p, err := plan.FromReply(reply, plannerRequest(s), time.Now())
	if errors.Is(err, plan.ErrNoPlan) {
		return nil
	}
	if err != nil {
		s.entries = append(s.entries, chatEntry{roleError, "Couldn't read the proposed plan: " + err.Error() + ". Ask the planner to write it again."})
		return nil
	}
	p.Base = m.base
	p.Orchestrated = m.planWatch == config.WatchPlanner
	var cmds []tea.Cmd
	kept := m.plans[:0]
	for _, old := range m.plans {
		if old.Status == plan.Draft {
			cmds = append(cmds, m.deletePlan(old.ID))
			continue
		}
		kept = append(kept, old)
	}
	m.plans = append([]*plan.Plan{p}, kept...)
	cmds = append(cmds, m.assignDrafting(s, p))
	s.entries = append(s.entries, chatEntry{roleNote, fmt.Sprintf("Proposed plan: %s · %s. Press ctrl+o to review and start it.", p.Title, plural(len(p.Tasks), "task"))})
	cmds = append(cmds, m.savePlan(p))
	if m.focus == focusChat && m.chatSession() == s {
		m.review = newReview(p)
	} else {
		cmds = append(cmds, m.notify("✦ The planner proposed a plan; press P to review it", toastInfo))
	}
	cmds = append(cmds, m.alert("The planner proposed a plan: "+p.Title))
	return tea.Batch(cmds...)
}

// draft is the plan waiting for review, if any.
func (m *Model) draft() *plan.Plan {
	for _, p := range m.plans {
		if p.Status == plan.Draft {
			return p
		}
	}
	return nil
}

func (m *Model) openReview() tea.Cmd {
	d := m.draft()
	if d == nil {
		return m.notify("No plan to review yet: ask the planner for one", toastInfo)
	}
	m.review = newReview(d)
	return nil
}

func (m *Model) planByID(id string) *plan.Plan {
	for _, p := range m.plans {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func planFile(id string) string { return "plans/" + id + ".json" }

// savePlan stamps p as changed and writes it with the sessions: marshalled
// on the UI goroutine, so later edits can't race the write, and written
// again on quit if the background write hadn't happened yet.
func (m *Model) savePlan(p *plan.Plan) tea.Cmd {
	p.Updated = time.Now()
	return tea.Batch(m.persistSessions(), m.writeGroupInfo(p))
}

func (m *Model) deletePlan(id string) tea.Cmd {
	delete(m.savedJSON, planFile(id))
	crews := m.dropCrews(id)
	if m.store == nil {
		return crews
	}
	st := m.store
	return tea.Batch(crews, func() tea.Msg { return persistMsg{err: plan.Delete(st, id)} })
}

func (m *Model) loadPlans() tea.Cmd {
	if m.store == nil {
		return nil
	}
	st := m.store
	return func() tea.Msg {
		plans, err := plan.List(st)
		return plansLoadedMsg{plans, err}
	}
}

func (m *Model) onPlansLoaded(msg plansLoadedMsg) tea.Cmd {
	m.plans = append(m.plans, msg.plans...)
	for _, p := range msg.plans {
		if b, err := json.Marshal(p); err == nil {
			m.savedJSON[planFile(p.ID)] = b
		}
	}
	for _, p := range m.plans {
		for i := range p.Tasks {
			// grove closed before this task's worktree was made.
			if t := &p.Tasks[i]; t.Status == plan.Working && t.Path == "" {
				t.Status = plan.Queued
			}
		}
	}
	if m.snap != nil {
		// Their groups gather worktrees in the list.
		m.rebuildRows()
	}
	m.plansLoaded = true
	cmds := []tea.Cmd{m.schedule(), m.pruneCrews()}
	if msg.err != nil {
		cmds = append(cmds, m.notify("Couldn't read some saved plans: "+msg.err.Error(), toastErr))
	}
	return tea.Batch(cmds...)
}

// checkPlan validates the reviewed plan against the branches and folders
// that exist right now, before anything is created.
func (m *Model) checkPlan(p *plan.Plan) tea.Cmd {
	repo, main, id := m.repo, m.plannerDir(), p.ID
	c := p.Clone()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
		defer cancel()
		b, err := git.ListBranches(ctx, repo)
		if err != nil {
			return planCheckedMsg{id: id, err: err}
		}
		probs := c.Validate(takenBranches(b))
		for _, t := range c.Tasks {
			if t.Branch == "" || main == "" {
				continue
			}
			dir := git.DefaultWorktreePath(main, repo.Name, t.Branch)
			if _, err := os.Stat(dir); err == nil {
				probs = append(probs, fmt.Sprintf("%s: the folder %s already exists.", t.Title, dir))
			}
		}
		return planCheckedMsg{id: id, problems: probs, branches: b}
	}
}

func (m *Model) onPlanChecked(msg planCheckedMsg) tea.Cmd {
	r := m.review
	if r == nil || r.p.ID != msg.id || !r.checking {
		return nil
	}
	r.checking = false
	switch {
	case msg.err != nil:
		r.problems = []string{"Couldn't list branches: " + msg.err.Error()}
		return nil
	case len(msg.problems) > 0:
		r.problems = msg.problems
		return nil
	}
	p := r.p
	return m.withAgent(func(k agent.Kind) tea.Cmd { return m.startPlan(p, k, msg.branches) })
}

// startPlan confirms the reviewed plan and starts its first tasks.
func (m *Model) startPlan(p *plan.Plan, k agent.Kind, branches git.Branches) tea.Cmd {
	p.Tidy()
	p.Status, p.Agent, p.Base = plan.Running, string(k), m.base
	for i := range p.Tasks {
		p.Tasks[i].Status = plan.Queued
	}
	for i, old := range m.plans {
		if old.ID == p.ID {
			m.plans[i] = p
		}
	}
	m.review = nil
	m.planBranches = branches
	m.planWatch = "" // asked again before the next plan
	watcher := "you watch its agents"
	if p.Orchestrated {
		watcher = "the planner watches its agents for you"
	}
	if m.planner != nil {
		m.planner.entries = append(m.planner.entries, chatEntry{roleNote, fmt.Sprintf("Started %s: %s, up to %d at a time; %s.", p.Title, plural(len(p.Tasks), "task"), m.cfg.Planner.Slots(), watcher)})
	}
	return tea.Batch(
		m.notify(fmt.Sprintf("Started %s · %s", p.Title, plural(len(p.Tasks), "task")), toastOK),
		m.savePlan(p),
		m.schedule(),
	)
}

func taskKey(p *plan.Plan, t *plan.Task) string { return p.ID + "/" + t.ID }

// busyTasks counts tasks holding an agent slot: their worktree is being
// made, their agent is working, or it is waiting for the user's approval.
func (m *Model) busyTasks() int {
	n := 0
	for _, p := range m.plans {
		for i := range p.Tasks {
			t := &p.Tasks[i]
			if t.Status != plan.Working {
				continue
			}
			if m.creatingTask[taskKey(p, t)] {
				n++
				continue
			}
			if s := m.sessionFor(t.Path); t.Path != "" && s != nil && (s.running || len(s.pending) > 0) {
				n++
			}
		}
	}
	return n
}

// schedule starts queued tasks while agent slots are free, oldest plan
// first.
func (m *Model) schedule() tea.Cmd {
	main := m.plannerDir()
	if main == "" {
		return nil // no scan yet; onScan calls again
	}
	free := m.cfg.Planner.Slots() - m.busyTasks()
	var cmds []tea.Cmd
	for i := len(m.plans) - 1; i >= 0 && free > 0; i-- {
		p := m.plans[i]
		if p.Status != plan.Running {
			continue
		}
		for j := range p.Tasks {
			if t := &p.Tasks[j]; t.Status == plan.Queued && free > 0 {
				free--
				cmds = append(cmds, m.startTask(p, t, main))
			}
		}
	}
	return tea.Batch(cmds...)
}

// startTask creates a task's worktree, copying the files [new] copy names,
// as the new-worktree dialog does. Its agent starts once that is done.
func (m *Model) startTask(p *plan.Plan, t *plan.Task, main string) tea.Cmd {
	t.Status = plan.Working
	m.creatingTask[taskKey(p, t)] = true
	repo, branches, patterns := m.repo, m.planBranches, m.cfg.New.Copy
	opt := git.AddOptions{Path: git.DefaultWorktreePath(main, repo.Name, t.Branch), Branch: t.Branch, From: p.Base}
	pid, tid := p.ID, t.ID
	create := func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Minute)
		defer cancel()
		branch, err := git.AddWorktree(ctx, repo, branches, opt)
		msg := taskCreatedMsg{plan: pid, task: tid, path: opt.Path, branch: branch, err: err}
		if err == nil {
			_, msg.copyErr = git.CopyInto(main, opt.Path, git.MatchCopy(main, patterns))
		}
		return msg
	}
	return tea.Batch(create, m.savePlan(p), m.startAnimating())
}

func (m *Model) onTaskCreated(msg taskCreatedMsg) tea.Cmd {
	p := m.planByID(msg.plan)
	if p == nil {
		return nil
	}
	t, ok := p.Task(msg.task)
	if !ok {
		return nil
	}
	delete(m.creatingTask, taskKey(p, t))
	if msg.err != nil {
		t.Status, t.Error = plan.Failed, "couldn't create its worktree: "+msg.err.Error()
		m.plannerNote(roleError, fmt.Sprintf("%s: %s", t.Title, t.Error))
		return tea.Batch(m.notify(t.Title+": "+t.Error, toastErr), m.finishIfSettled(p), m.savePlan(p), m.schedule())
	}
	t.Path = msg.path
	s := m.sessionFor(msg.path)
	if s == nil {
		s = m.newSession(msg.path, msg.branch, agent.Kind(p.Agent))
	}
	s.seen = false
	cmds := []tea.Cmd{m.savePlan(p), m.startScan(), m.send(s, "Start on your task: "+t.Title+".")}
	if msg.copyErr != nil {
		cmds = append(cmds, m.notify("Copying files into "+msg.branch+" failed: "+msg.copyErr.Error(), toastErr))
	}
	return tea.Batch(cmds...)
}

// taskFor finds the plan task worked on in the worktree at path.
func (m *Model) taskFor(path string) (*plan.Plan, *plan.Task) {
	if path == "" {
		return nil, nil
	}
	for _, p := range m.plans {
		if p.Status == plan.Draft {
			continue
		}
		for i := range p.Tasks {
			if t := &p.Tasks[i]; t.Path != "" && git.SamePath(t.Path, path) {
				return p, t
			}
		}
	}
	return nil, nil
}

// onTaskTurn updates a task when its agent's turn ends, and starts queued
// tasks if a slot came free. A reply that ends with a question keeps the
// task working until the user answers.
func (m *Model) onTaskTurn(s *session, ev agent.Event) tea.Cmd {
	p, t := m.taskFor(s.path)
	if t == nil {
		return nil
	}
	again := t.Status == plan.Finished
	// While grove runs the agent's git for it, the agent carries on with
	// the output.
	if t.Status == plan.Working && len(s.pending) == 0 && !m.gitBusy(s) {
		switch {
		case ev.Err != nil:
			t.Status, t.Error = plan.Failed, ev.Err.Error()
		case !endsWithQuestion(s.lastReply.String()):
			t.Status, t.Error = plan.Finished, ""
		}
	}
	if fix, ok := m.fixTurnEnded(p, t); ok {
		// grove commits the resolved merge and merges the plan again.
		return tea.Batch(fix, m.savePlan(p), m.schedule())
	}
	if t.Status == plan.Finished && watched(p) {
		m.progressed()
	}
	queued := m.queueTurn(s, again)
	return tea.Batch(queued, m.finishIfSettled(p), m.savePlan(p), m.schedule(), m.orchestrate())
}

func endsWithQuestion(reply string) bool {
	return strings.HasSuffix(strings.TrimRight(reply, " \n\t*_`"), "?")
}

// finishIfSettled marks a plan done once every task has finished or failed,
// and points the user to merging. A plan the planner watches stays running
// for the planner to merge instead.
func (m *Model) finishIfSettled(p *plan.Plan) tea.Cmd {
	if p.Status != plan.Running || !p.Settled() {
		return nil
	}
	if watched(p) {
		if !m.mergingPlan[p.ID] && (p.Merge == nil || p.Merge.Fixing == "") {
			m.queueSettled(p)
		}
		return nil
	}
	p.Status = plan.Done
	failed := 0
	for _, t := range p.Tasks {
		if t.Status == plan.Failed {
			failed++
		}
	}
	text := fmt.Sprintf("%s is done: %s finished", p.Title, plural(len(p.Tasks)-failed, "task"))
	if failed > 0 {
		text += fmt.Sprintf(", %d failed", failed)
	}
	m.plannerNote(roleNote, text+". Review each worktree, then press c on the one to merge into.")
	return tea.Batch(m.notify("✦ "+text, toastOK), m.alert(text))
}

func (m *Model) plannerNote(role chatRole, text string) {
	if m.planner != nil {
		m.planner.entries = append(m.planner.entries, chatEntry{role, text})
	}
}

// taskContext tells a task's agent what it is for and what the others are
// doing, appended to its worktree briefing.
func (m *Model) taskContext(s *session) string {
	p, t := m.taskFor(s.path)
	if t == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Your task\n\nThis worktree was made for one task of the plan %q. Do this task only; the plan's other tasks are being done by other agents in their own worktrees.\n\n", p.Title)
	fmt.Fprintf(&b, "Task: %s\n\n%s\n", t.Title, t.Brief)
	var others []string
	for _, o := range p.Tasks {
		if o.ID != t.ID {
			others = append(others, fmt.Sprintf("- %s (%s)", o.Title, o.Branch))
		}
	}
	if len(others) > 0 {
		b.WriteString("\nThe plan's other tasks, for context:\n" + strings.Join(others, "\n") + "\n")
	}
	b.WriteString(m.groupContext(p, t))
	if m.groveBin != "" {
		b.WriteString("\nThe plan also has a plan chat for the user, the planner and every task's agent, through the plan tools (`members`, `post`, `read`). " +
			"The user gives feedback on the whole plan there. Read it when you are woken for a message in it; post there only for what the user or the whole plan needs to know.\n")
	}
	b.WriteString("\nWhen the task is done, say so plainly and list what you changed. If you need a decision from the user, end your reply with the question.\n")
	return b.String()
}
