package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/shell"
)

// The watching planner merges a plan for the user once its tasks are
// settled: grove commits each finished task's worktree and merges their
// branches, in plan order, into the plan's own worktree (plan.Merge, a new
// branch from the plan's base), where the user tries the result. The plan
// is then in review, and the planner waits for the user's feedback in the
// plan chat: changes reopen tasks, and merging again brings in what they
// did. A branch that conflicts with the work merged so far isn't merged:
// grove merges the plan's branch into that task's worktree and asks its
// agent to resolve the conflict, commits the merge when it is done, and
// merges the plan again. Merging is all grove's own git work; the planner
// only asks for it (decisions.Merge, or the merge control tool).

// planMergedMsg is the outcome of merging a plan's tasks.
type planMergedMsg struct {
	plan           string
	branch, path   string   // the plan's worktree, once there is one
	merged, had    []string // task branches merged now, and those it had already
	conflict       string   // the task branch that conflicts, if any
	conflictPaths  []string // and its files
	err            error
	createdNow     bool
	copyErr        error
	committedTasks []string // tasks whose uncommitted work was committed first
}

// testsDoneMsg is how the test command went in a plan's worktree.
type testsDoneMsg struct {
	plan, command string
	report        string // what was merged, for the planner
	res           shell.Result
}

// fixStartedMsg is the plan's branch merged into a conflicting task's
// worktree, for its agent to resolve.
type fixStartedMsg struct {
	plan, task string
	paths      []string
	err        error
}

// fixDoneMsg is the outcome of committing a task's resolved merge.
type fixDoneMsg struct {
	plan, task string
	left       []string // files still holding conflict markers
	err        error
}

// mergedInto finds the plan whose finished tasks are merged in the worktree
// at path.
func (m *Model) mergedInto(path string) *plan.Plan {
	if path == "" {
		return nil
	}
	for _, p := range m.plans {
		if p.Merge != nil && p.Merge.Path != "" && git.SamePath(p.Merge.Path, path) {
			return p
		}
	}
	return nil
}

// latestStarted is the newest plan that has started and isn't done, else
// the newest started one.
func (m *Model) latestStarted() *plan.Plan {
	var done *plan.Plan
	for i := len(m.plans) - 1; i >= 0; i-- {
		switch p := m.plans[i]; p.Status {
		case plan.Draft:
		case plan.Done:
			if done == nil {
				done = p
			}
		default:
			return p
		}
	}
	return done
}

// findPlan finds a plan by ID or title.
func (m *Model) findPlan(name string) *plan.Plan {
	name = strings.TrimSpace(name)
	for _, p := range m.plans {
		if p.ID == name || strings.EqualFold(p.Title, name) {
			return p
		}
	}
	return nil
}

// reviewAsk tells the planner what to do with a merged plan whose tests
// passed, or that it tests itself.
const reviewAsk = "Then post a short summary for the user in the plan chat: what was built, how to try it, and anything that needs their attention, " +
	"and wait for their feedback there. When they ask for changes, reply to the agents of the tasks that own that part, which reopens them, " +
	"and merge again once they are finished; when the user is happy, close the plan."

// testFixAsk tells the planner what to do when a merged plan's tests fail.
const testFixAsk = "Don't ask the user yet. Work out which task's work causes each failure: read the output, the merged code and the tasks' diffs, " +
	"and run the tests yourself in the worktree if that helps, without changing files. Reply to those tasks' agents with what fails and how to see it; " +
	"once they are finished, merge again, and grove tests again. Tell the user in the plan chat only if the agents can't fix it, " +
	"or the failure isn't caused by the plan's work (for example it fails on the base branch too, or the test command is wrong)."

// mergeBlocker says why p can't be merged now, or "".
func (m *Model) mergeBlocker(p *plan.Plan) string {
	switch {
	case p.Status != plan.Running && p.Status != plan.Review:
		return "it isn't running"
	case m.mergingPlan[p.ID]:
		return "grove is merging it already"
	case m.testingPlan[p.ID]:
		return "grove is running its tests; you'll hear how they went"
	case p.Merge != nil && p.Merge.Fixing != "":
		return p.Merge.Fixing + "'s agent is still resolving a conflict; grove merges again once it is done"
	case !p.Settled():
		return "some tasks are still working or waiting; merge once every task is finished or failed"
	case m.plannerDir() == "":
		return "grove hasn't found the main worktree yet"
	}
	for _, t := range p.Tasks {
		if s := m.sessionFor(t.Path); t.Path != "" && s != nil && s.running {
			return t.Branch + "'s agent is working"
		}
	}
	if !slices.ContainsFunc(p.Tasks, func(t plan.Task) bool { return t.Status == plan.Finished && t.Path != "" }) {
		return "no task finished"
	}
	return ""
}

// mergePlan commits each finished task's work and merges the branches into
// the plan's worktree, making it first if need be.
func (m *Model) mergePlan(p *plan.Plan) tea.Cmd {
	if why := m.mergeBlocker(p); why != "" {
		m.queueMerge(p, "grove couldn't merge the plan: "+why+".")
		return m.orchestrate()
	}
	m.mergingPlan[p.ID] = true
	type taskWork struct{ branch, path, message string }
	var tasks []taskWork
	for _, t := range p.Tasks {
		if t.Status == plan.Finished && t.Path != "" {
			msg := m.suggestedCommitMessage(t.Path)
			if msg == "" {
				msg = t.Title
			}
			tasks = append(tasks, taskWork{t.Branch, t.Path, msg})
		}
	}
	repo, main, patterns, base, id := m.repo, m.plannerDir(), m.cfg.New.Copy, p.Base, p.ID
	var branch, path string
	if p.Merge != nil {
		branch, path = p.Merge.Branch, p.Merge.Path
	}
	slug := planSlug(p)
	m.plannerNote(roleNote, "Merging "+p.Title+"…")
	merge := func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		defer cancel()
		out := planMergedMsg{plan: id, branch: branch, path: path}
		copied := git.MatchCopy(main, patterns)
		var exclude []string
		for _, c := range copied {
			exclude = append(exclude, filepath.ToSlash(c))
		}
		if _, err := os.Stat(path); path == "" || err != nil {
			b, err := git.ListBranches(ctx, repo)
			if err != nil {
				out.err = err
				return out
			}
			if branch == "" {
				branch = freeBranch(slug, takenBranches(b), func(name string) bool {
					_, err := os.Stat(git.DefaultWorktreePath(main, repo.Name, name))
					return err == nil
				})
			}
			path = git.DefaultWorktreePath(main, repo.Name, branch)
			out.branch, out.path = branch, path
			if _, err := git.AddWorktree(ctx, repo, b, git.AddOptions{Path: path, Branch: branch, From: base}); err != nil {
				out.err = fmt.Errorf("couldn't make the plan's worktree: %w", err)
				out.path = ""
				return out
			}
			out.createdNow = true
			_, out.copyErr = git.CopyInto(main, path, copied)
		}
		if busy, err := git.MergeInProgress(ctx, path); err != nil || busy {
			out.err = errors.Join(err, errors.New("a merge is already in progress in "+path))
			return out
		}
		if dirty, err := git.HasTrackedChanges(ctx, path); err != nil || dirty {
			out.err = errors.Join(err, errors.New(path+" has uncommitted changes; commit or discard them before merging again"))
			return out
		}
		for _, t := range tasks {
			committed, err := git.CommitWork(ctx, t.path, t.message, exclude)
			if err != nil {
				out.err = fmt.Errorf("couldn't commit %s's work: %w", t.branch, err)
				return out
			}
			if committed {
				out.committedTasks = append(out.committedTasks, t.branch)
			}
			n, err := git.CommitsBetween(ctx, path, "HEAD", t.branch)
			if err != nil {
				out.err = err
				return out
			}
			if n == 0 {
				out.had = append(out.had, t.branch)
				continue
			}
			paths, err := git.Merge(ctx, path, t.branch)
			if err != nil {
				out.err = fmt.Errorf("couldn't merge %s: %w", t.branch, err)
				return out
			}
			if len(paths) > 0 {
				out.conflict, out.conflictPaths = t.branch, paths
				if err := git.AbortMerge(ctx, path); err != nil {
					out.err = fmt.Errorf("couldn't abort the merge of %s: %w", t.branch, err)
				}
				return out
			}
			out.merged = append(out.merged, t.branch)
		}
		return out
	}
	return tea.Batch(merge, m.startAnimating())
}

// planSlug names a plan's branch after its title.
func planSlug(p *plan.Plan) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(p.Title) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = p.ID
	}
	return "plan/" + s
}

// freeBranch is name, or name-2, name-3…, whichever isn't a branch yet and
// whose folder isn't taken.
func freeBranch(name string, taken []string, folderTaken func(string) bool) string {
	for i := 1; ; i++ {
		try := name
		if i > 1 {
			try = fmt.Sprintf("%s-%d", name, i)
		}
		if !slices.ContainsFunc(taken, func(b string) bool { return strings.EqualFold(b, try) }) && !folderTaken(try) {
			return try
		}
	}
}

func (m *Model) onPlanMerged(msg planMergedMsg) tea.Cmd {
	delete(m.mergingPlan, msg.plan)
	p := m.planByID(msg.plan)
	if p == nil {
		return nil
	}
	if msg.branch != "" && msg.path != "" {
		if p.Merge == nil {
			p.Merge = &plan.Merge{}
		}
		p.Merge.Branch, p.Merge.Path = msg.branch, msg.path
	}
	cmds := []tea.Cmd{m.savePlan(p), m.startScan()}
	if msg.copyErr != nil {
		cmds = append(cmds, m.notify("Copying files into "+msg.branch+" failed: "+msg.copyErr.Error(), toastErr))
	}
	committed := ""
	if len(msg.committedTasks) > 0 {
		committed = " grove committed the uncommitted work of " + strings.Join(msg.committedTasks, ", ") + " first."
	}
	switch {
	case msg.err != nil:
		text := fmt.Sprintf("Couldn't merge %s: %v", p.Title, msg.err)
		m.plannerNote(roleError, text)
		m.queueMerge(p, text+"."+committed)
		cmds = append(cmds, m.notify(text, toastErr), m.alert(text), m.grovePost(p, text))
	case msg.conflict != "":
		t := planTask(p, msg.conflict)
		if t == nil {
			return tea.Batch(cmds...)
		}
		merged := ""
		if len(msg.merged) > 0 {
			merged = " after merging " + strings.Join(msg.merged, ", ")
		}
		text := fmt.Sprintf("%s conflicts with the work merged so far%s, in %s. Its agent is resolving it; grove merges the plan again once it is done.",
			t.Branch, merged, strings.Join(msg.conflictPaths, ", "))
		m.plannerNote(roleNote, text)
		p.Merge.Fixing = t.Branch
		t.Status, p.Status = plan.Working, plan.Running
		path, planBranch, pid, tid := t.Path, p.Merge.Branch, p.ID, t.ID
		cmds = append(cmds, m.grovePost(p, text), func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
			defer cancel()
			paths, err := git.Merge(ctx, path, planBranch)
			return fixStartedMsg{plan: pid, task: tid, paths: paths, err: err}
		})
	default:
		p.Status = plan.Review
		var left []string
		for _, t := range p.Tasks {
			if t.Status == plan.Failed {
				left = append(left, t.Branch)
			}
		}
		text := fmt.Sprintf("Merged %s into %s, in %s.", plural(len(msg.merged)+len(msg.had), "task"), msg.branch, msg.path)
		if len(left) > 0 {
			text += " Left out, as they failed: " + strings.Join(left, ", ") + "."
		}
		m.plannerNote(roleNote, text)
		ask := text + committed
		if len(msg.merged) > 0 {
			ask += " Merged now: " + strings.Join(msg.merged, ", ") + "."
		}
		if len(msg.had) > 0 {
			ask += " Already merged before: " + strings.Join(msg.had, ", ") + "."
		}
		p.Merge.Tests = ""
		if test := m.testCommand(p); test != "" {
			cmds = append(cmds, m.grovePost(p, text+" Running the tests: `"+test+"`."), m.runPlanTests(p, test, ask))
			break
		}
		m.queueMerge(p, ask+" No test command is set ([planner] test in grove's config, or \"test\" in the plan): "+
			"work out how this repository is built and tested, and run that in the worktree, without changing files. "+reviewAsk)
		cmds = append(cmds, m.grovePost(p, text+" Try it there, then tell the planner here what to change."), m.waitForFeedback(p))
	}
	return tea.Batch(append(cmds, m.orchestrate())...)
}

func (m *Model) onFixStarted(msg fixStartedMsg) tea.Cmd {
	p := m.planByID(msg.plan)
	if p == nil || p.Merge == nil {
		return nil
	}
	t, ok := p.Task(msg.task)
	if !ok {
		return nil
	}
	s := m.sessionFor(t.Path)
	switch {
	case msg.err != nil || s == nil:
		p.Merge.Fixing = ""
		why := "its agent is gone"
		if msg.err != nil {
			why = msg.err.Error()
		}
		t.Status, t.Error = plan.Failed, "couldn't merge "+p.Merge.Branch+" into it to resolve a conflict: "+why
		m.queueMerge(p, t.Branch+" failed: "+t.Error+".")
		return tea.Batch(m.savePlan(p), m.notify(t.Title+": "+t.Error, toastErr), m.orchestrate())
	case len(msg.paths) == 0:
		// It merged cleanly after all, so the plan merges again.
		p.Merge.Fixing = ""
		t.Status = plan.Finished
		return tea.Batch(m.savePlan(p), m.mergePlan(p))
	}
	return tea.Batch(m.savePlan(p), m.tell(s, fmt.Sprintf("Your branch has to be merged with the plan's other finished work, which is on the branch %s. "+
		"grove has started merging %s into your worktree, and it conflicts in: %s. "+
		"Resolve the conflict markers in those files so that both your task and the merged work keep working, and check that it builds. "+
		"Don't run git yourself: when you say you are done, grove commits the merge and merges the plan again.",
		p.Merge.Branch, p.Merge.Branch, strings.Join(msg.paths, ", "))))
}

// fixTurnEnded commits a resolved conflict once the task's agent says it
// is done, and reports whether the task was fixing one.
func (m *Model) fixTurnEnded(p *plan.Plan, t *plan.Task) (tea.Cmd, bool) {
	if p.Merge == nil || p.Merge.Fixing != t.Branch || t.Status != plan.Finished {
		return nil, false
	}
	main, patterns, path, pid, tid := m.plannerDir(), m.cfg.New.Copy, t.Path, p.ID, t.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
		defer cancel()
		var exclude []string
		for _, c := range git.MatchCopy(main, patterns) {
			exclude = append(exclude, filepath.ToSlash(c))
		}
		left, err := git.ConcludeMerge(ctx, path, exclude)
		return fixDoneMsg{plan: pid, task: tid, left: left, err: err}
	}, true
}

func (m *Model) onFixDone(msg fixDoneMsg) tea.Cmd {
	p := m.planByID(msg.plan)
	if p == nil || p.Merge == nil {
		return nil
	}
	t, ok := p.Task(msg.task)
	if !ok || p.Merge.Fixing != t.Branch {
		return nil
	}
	s := m.sessionFor(t.Path)
	switch {
	case msg.err != nil:
		p.Merge.Fixing = ""
		t.Status, t.Error = plan.Failed, "couldn't commit its resolved merge: "+msg.err.Error()
		m.queueMerge(p, t.Branch+" failed: "+t.Error+".")
		return tea.Batch(m.savePlan(p), m.notify(t.Title+": "+t.Error, toastErr), m.orchestrate())
	case len(msg.left) > 0 && s != nil:
		t.Status = plan.Working
		return tea.Batch(m.savePlan(p), m.tell(s, "These files still have conflict markers: "+strings.Join(msg.left, ", ")+
			". Resolve them all, then say you are done."))
	}
	p.Merge.Fixing = ""
	m.plannerNote(roleNote, t.Branch+" resolved its conflict; merging "+p.Title+" again.")
	return tea.Batch(m.savePlan(p), m.mergePlan(p))
}

// closePlan ends a plan in review: the user is happy with the merge.
func (m *Model) closePlan(p *plan.Plan, note string) tea.Cmd {
	if p.Status != plan.Review && p.Status != plan.Running {
		return nil
	}
	p.Status = plan.Done
	text := p.Title + " is closed"
	if note = strings.TrimSpace(note); note != "" {
		text += ": " + note
	}
	text += "."
	if p.Merge != nil {
		text += " Its work is on " + p.Merge.Branch + ", in " + p.Merge.Path + "; ship it from there (s)."
	}
	m.plannerNote(roleNote, text)
	return tea.Batch(m.savePlan(p), m.grovePost(p, text), m.notify("✦ "+p.Title+" is closed", toastOK))
}

// planTask finds a plan's task by branch.
func planTask(p *plan.Plan, branch string) *plan.Task {
	for i := range p.Tasks {
		if p.Tasks[i].Branch == branch {
			return &p.Tasks[i]
		}
	}
	return nil
}

// grovePost posts grove's own news in a plan's chat.
func (m *Model) grovePost(p *plan.Plan, text string) tea.Cmd {
	log, ok := m.groupLog(p, plan.ChatID)
	if !ok {
		return nil
	}
	return func() tea.Msg {
		if err := log.Post(group.Message{From: group.Grove, Text: text}); err != nil {
			return actionMsg{err: fmt.Errorf("couldn't post in the plan chat: %w", err)}
		}
		return groupPostedMsg{}
	}
}

// queueMerge tells the watching planner how merging a plan went.
func (m *Model) queueMerge(p *plan.Plan, text string) {
	if watched(p) {
		m.orch.queue = append(m.orch.queue, orchItem{kind: orchMerge, plan: p.ID, lines: []string{text}, urgent: true})
	}
}

// queueSettled asks the watching planner to merge a plan whose tasks are
// all finished or failed, once.
func (m *Model) queueSettled(p *plan.Plan) {
	if !watched(p) || slices.ContainsFunc(m.orch.queue, func(it orchItem) bool { return it.kind == orchSettled && it.plan == p.ID }) {
		return
	}
	m.orch.queue = append(m.orch.queue, orchItem{kind: orchSettled, plan: p.ID, urgent: true})
}

// testCommand is the command that tests p's merge: the config's, else the
// one the planner proposed with the plan.
func (m *Model) testCommand(p *plan.Plan) string {
	if t := strings.TrimSpace(m.cfg.Planner.Test); t != "" {
		return t
	}
	return strings.TrimSpace(p.Test)
}

// runPlanTests runs the test command in p's worktree.
func (m *Model) runPlanTests(p *plan.Plan, command, report string) tea.Cmd {
	m.testingPlan[p.ID] = true
	m.plannerNote(roleNote, "Testing "+p.Title+": "+command)
	dir, id, timeout := p.Merge.Path, p.ID, m.cfg.Planner.TestTimeout()
	run := func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, timeout)
		defer cancel()
		return testsDoneMsg{plan: id, command: command, report: report, res: shell.Run(ctx, dir, command, 8000)}
	}
	return tea.Batch(run, m.startAnimating())
}

func (m *Model) onTestsDone(msg testsDoneMsg) tea.Cmd {
	delete(m.testingPlan, msg.plan)
	p := m.planByID(msg.plan)
	if p == nil || p.Merge == nil {
		return nil
	}
	res := msg.res
	if res.OK() {
		p.Merge.Tests = plan.TestsPassed
		m.plannerNote(roleNote, "The tests passed.")
		m.queueMerge(p, msg.report+" grove ran `"+msg.command+"` in that worktree and it passed. "+
			"Check anything else the change needs, if you must, without changing files. "+reviewAsk)
		return tea.Batch(m.savePlan(p), m.grovePost(p, "The tests passed. Try it there, then tell the planner here what to change."),
			m.waitForFeedback(p), m.orchestrate())
	}
	p.Merge.Tests = plan.TestsFailed
	var why string
	switch {
	case res.TimedOut:
		why = fmt.Sprintf("ran longer than %s and was stopped", m.cfg.Planner.TestTimeout())
	case res.Err != nil:
		why = "couldn't run: " + res.Err.Error()
	default:
		why = fmt.Sprintf("failed with exit code %d", res.ExitCode)
	}
	output := strings.TrimSpace(strings.ToValidUTF8(string(res.Output), "?"))
	if output == "" {
		output = "(no output)"
	}
	m.plannerNote(roleNote, "The tests "+why+".")
	m.queueMerge(p, fmt.Sprintf("%s grove ran `%s` in that worktree and it %s. The end of its output:\n\n```\n%s\n```\n\n%s",
		msg.report, msg.command, why, output, testFixAsk))
	lines := strings.Split(output, "\n")
	if len(lines) > 15 {
		lines = append([]string{"…"}, lines[len(lines)-15:]...)
	}
	post := fmt.Sprintf("The tests (`%s`) %s. The planner is getting the tasks' agents to fix it before asking you.\n\n```\n%s\n```",
		msg.command, why, strings.Join(lines, "\n"))
	return tea.Batch(m.savePlan(p), m.grovePost(p, post),
		m.notify(p.Title+": the merged tests failed; the planner is sending the failures to the agents", toastInfo), m.orchestrate())
}

// waitForFeedback tells the user a merged plan is ready for them.
func (m *Model) waitForFeedback(p *plan.Plan) tea.Cmd {
	title := p.Title + " is merged into " + p.Merge.Branch + " and waits for your feedback"
	return tea.Batch(m.notify("✦ "+title+" (t in its worktree)", toastOK), m.alert(title))
}
