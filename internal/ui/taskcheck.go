package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/gitpolicy"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/shell"
)

// A planner's turn waits for everything run in it, so grove runs the long
// checks for planners in the background, as it does a merged plan's tests:
// when a watched task finishes, the plan's test command runs in its
// worktree before the planner hears it is done (with the result), and the
// test control tool runs a command in a task's worktree and sends the
// planner that asked how it went. One run per worktree at a time; a run is
// cancelled when its task is reopened (for a finished task's tests) or its
// worktree goes.

// testOutput is how much of a test command's output grove keeps.
const testOutput = 8000

type checkKind uint8

const (
	checkFinished checkKind = iota // a finished task's tests, before orchFinished
	checkAsked                     // a planner's test order, reported as orchCheck
)

// taskCheck is a command grove runs in a task's worktree for a planner.
type taskCheck struct {
	kind                        checkKind
	plan, branch, path, command string
	reply                       string // the agent's closing reply, for checkFinished
	group                       string // the group of the planner that asked, for checkAsked
	seq                         int
	cancel                      context.CancelFunc
	done                        chan struct{} // closed once the command has ended
}

// checkDoneMsg is how a task check went.
type checkDoneMsg struct {
	key string
	seq int
	res shell.Result
}

// checkIn is the check running in the worktree at path, if any.
func (m *Model) checkIn(path string) *taskCheck {
	if path == "" {
		return nil
	}
	return m.checks[git.PathKey(path)]
}

// checkingFinished reports whether grove is testing a finished task of p.
func (m *Model) checkingFinished(p *plan.Plan) bool {
	for _, c := range m.checks {
		if c.kind == checkFinished && c.plan == p.ID {
			return true
		}
	}
	return false
}

// startCheck runs command in t's worktree in the background.
func (m *Model) startCheck(p *plan.Plan, t *plan.Task, kind checkKind, command, reply, group string) tea.Cmd {
	ctx, cancel := context.WithTimeout(m.ctx, m.cfg.Planner.TestTimeout())
	m.checkSeq++
	c := &taskCheck{kind: kind, plan: p.ID, branch: t.Branch, path: t.Path, command: command, reply: reply, group: group,
		seq: m.checkSeq, cancel: cancel, done: make(chan struct{})}
	key := git.PathKey(t.Path)
	m.checks[key] = c
	noteTo(m.checkOwner(c), roleNote, "grove is testing "+t.Branch+": "+command)
	run := func() tea.Msg {
		defer close(c.done)
		defer cancel()
		return checkDoneMsg{key: key, seq: c.seq, res: shell.Run(ctx, c.path, command, testOutput)}
	}
	return tea.Batch(run, m.startAnimating())
}

// checkOwner is the planner a check reports to: the one that asked, else
// the one in charge of the task. nil when it doesn't exist yet.
func (m *Model) checkOwner(c *taskCheck) *session {
	if c.kind == checkAsked {
		if c.group == "" {
			return m.planner
		}
		return m.crews[crewKey(c.plan, c.group)]
	}
	return m.ownerPlanner(c.path)
}

// testFinished hands a finished task to its planner: after its tests when
// the plan has a test command, else at once.
func (m *Model) testFinished(p *plan.Plan, t *plan.Task, reply string) tea.Cmd {
	item := orchItem{kind: orchFinished, plan: p.ID, branch: t.Branch, lines: []string{reply}, urgent: true}
	command := m.testCommand(p)
	switch c := m.checkIn(t.Path); {
	case command == "":
	case c != nil && c.kind == checkFinished:
		return nil // already on its way
	case c != nil:
		item.lines = append(item.lines, fmt.Sprintf("grove didn't run the tests: `%s`, which a planner asked for, is still running in its worktree, and you'll get its result.", c.command))
	default:
		return m.startCheck(p, t, checkFinished, command, reply, "")
	}
	m.orch.queue = append(m.orch.queue, item)
	return nil
}

// decideTest starts a planner's test order.
func (m *Model) decideTest(by *session, tc testDecision) (tea.Cmd, outcome) {
	o := outcome{order: tc.order, what: "test " + tc.Branch, result: control.Applied}
	p, t, s := m.watchedTask(tc.Branch)
	command := strings.TrimSpace(tc.Command)
	switch {
	case t == nil || s == nil:
		o.result, o.detail = control.NotFound, "no watched task has that branch, or it hasn't started"
		return nil, o
	case m.checkIn(t.Path) != nil:
		o.result, o.detail = control.Failed, fmt.Sprintf("grove is already running `%s` in its worktree; wait for its result", m.checkIn(t.Path).command)
		return nil, o
	case command == "" && m.testCommand(p) == "":
		o.result, o.detail = control.Failed, "the plan has no test command; give the command to run"
		return nil, o
	case command == "":
		command = m.testCommand(p)
	default:
		if d := m.classifyFor(s, gitpolicy.Bash, command); d.Verdict != gitpolicy.NotGit {
			o.result, o.detail = control.Failed, "test doesn't run git ("+d.Reason+"); run it yourself"
			return nil, o
		}
	}
	group := ""
	if by != nil {
		group = by.crewGroup
	}
	o.detail = fmt.Sprintf("started `%s` in %s; grove sends you the result when it ends", command, t.Path)
	return m.startCheck(p, t, checkAsked, command, "", group), o
}

// pruneChecks cancels the checks whose result nobody needs any more: the
// task was reopened, its plan is no longer watched, or its worktree is gone.
func (m *Model) pruneChecks() {
	for key, c := range m.checks {
		p, t := m.taskFor(c.path)
		if t != nil && watched(p) && t.Branch == c.branch && (c.kind != checkFinished || t.Status == plan.Finished) && m.worktreeExists(c.path) {
			continue
		}
		c.cancel()
		delete(m.checks, key)
	}
}

// worktreeExists reports whether the last scan saw a worktree at path;
// true before the first scan.
func (m *Model) worktreeExists(path string) bool {
	if m.snap == nil {
		return true
	}
	for _, st := range m.snap.States {
		if git.SamePath(st.Path, path) {
			return true
		}
	}
	return false
}

// stopCheck cancels the check in the worktree at path and returns a wait
// for it to end, so the folder is free to delete.
func (m *Model) stopCheck(path string) func() {
	c := m.checkIn(path)
	if c == nil {
		return func() {}
	}
	c.cancel()
	delete(m.checks, git.PathKey(path))
	return func() {
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
		}
	}
}

func (m *Model) onCheckDone(msg checkDoneMsg) tea.Cmd {
	c := m.checks[msg.key]
	if c == nil || c.seq != msg.seq {
		return nil // cancelled
	}
	delete(m.checks, msg.key)
	p, t := m.taskFor(c.path)
	if t == nil || !watched(p) || t.Branch != c.branch || c.kind == checkFinished && t.Status != plan.Finished {
		return nil
	}
	res := msg.res
	output := strings.TrimSpace(strings.ToValidUTF8(string(res.Output), "?"))
	var text string
	if res.OK() {
		text = fmt.Sprintf("grove ran `%s` in its worktree and it passed.", c.command)
	} else {
		if output == "" {
			output = "(no output)"
		}
		text = fmt.Sprintf("grove ran `%s` in its worktree and it %s. The end of its output:\n\n```\n%s\n```", c.command, m.whyFailed(res), output)
	}
	noteTo(m.checkOwner(c), roleOutput, testEntry(c.command+" in "+c.branch, res.OK(), output))
	switch c.kind {
	case checkFinished:
		m.orch.queue = append(m.orch.queue, orchItem{kind: orchFinished, plan: p.ID, branch: t.Branch, lines: []string{c.reply, text}, urgent: true})
	case checkAsked:
		m.orch.queue = append(m.orch.queue, orchItem{kind: orchCheck, plan: p.ID, branch: t.Branch, group: c.group, lines: []string{text}, urgent: true})
	}
	return tea.Batch(m.finishIfSettled(p), m.orchestrate())
}

// whyFailed says how a test command that didn't pass went.
func (m *Model) whyFailed(res shell.Result) string {
	switch {
	case res.TimedOut:
		return fmt.Sprintf("ran longer than %s and was stopped", m.cfg.Planner.TestTimeout())
	case res.Err != nil:
		return "couldn't run: " + res.Err.Error()
	}
	return fmt.Sprintf("failed with exit code %d", res.ExitCode)
}
