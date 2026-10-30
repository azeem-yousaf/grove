package ui

import (
	"context"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/shell"
	"github.com/azeem-yousaf/grove/internal/usage"
)

// Command cards (pendingCommand: grove-run lines grove didn't run itself)
// of a watched task go to the planner with its approvals, unless only the
// user may answer them. Each has an id like an approval's. Approving one
// runs it in the task's worktree in the background, under the git guard,
// and gives the agent the output (the user's "run" hands over the
// terminal instead, which would stop grove until someone pressed Enter);
// denying dismisses it and tells the agent.

// cardTimeout bounds a command the planner approved.
const cardTimeout = 10 * time.Minute

// cardNote marks a command card wherever the planner sees it.
const cardNote = " (asks grove to run it: approving runs it in its worktree)"

// cardSuffix is cardNote when id is one of s's command cards.
func cardSuffix(s *session, id string) string {
	if s == nil {
		return ""
	}
	for _, a := range s.pending {
		if a.id == id && a.kind == pendingCommand {
			return cardNote
		}
	}
	return ""
}

// findCard finds a watched task's pending command card by ID, else by
// branch and command.
func (m *Model) findCard(branch, id, command string) (*session, int) {
	var sessions []*session
	if id != "" {
		for _, p := range m.plans {
			if !watched(p) {
				continue
			}
			for _, t := range p.Tasks {
				if s := m.sessionFor(t.Path); t.Path != "" && s != nil {
					sessions = append(sessions, s)
				}
			}
		}
	} else if _, _, s := m.watchedTask(branch); s != nil {
		sessions = append(sessions, s)
	}
	for _, s := range sessions {
		for i, a := range s.pending {
			if a.kind != pendingCommand {
				continue
			}
			if id != "" && a.id == id || id == "" && command != "" && control.NormCommand(a.text) == control.NormCommand(command) {
				return s, i
			}
		}
	}
	return nil, -1
}

// decideCard carries out the planner's answer to s.pending[i], a command
// card, reporting whether the agent should be resumed for it.
func (m *Model) decideCard(s *session, i int, approve bool, why string, o outcome) (tea.Cmd, outcome, bool) {
	a := s.pending[i]
	if approve {
		// grove's policy for the whole line, not only plain git.
		if pol := m.classify(s, "Bash", a.text); pol.userOnly && !a.policy.userOnly {
			s.pending[i].policy = pol
			a = s.pending[i]
		}
	}
	switch {
	case approve && a.policy.userOnly:
		o.result, o.detail = control.NeedsUser, a.policy.reason
		if o.detail == "" {
			o.detail = "only the user may run it"
		}
		return m.refuse(s, a), o, false
	case !approve:
		o.detail = "dismissed"
		return m.answerWith(s, i, false, byPlanner, why, ""), o, true
	}
	s.pending = append(s.pending[:i:i], s.pending[i+1:]...)
	s.answered = append(s.answered, answerRec{ID: a.id, Text: a.text, Approve: true, By: byPlanner})
	if n := len(s.answered); n > keepAnswers {
		s.answered = s.answered[n-keepAnswers:]
	}
	s.entries = append(s.entries, chatEntry{roleTool, "grove runs " + a.text + " (approved by the planner)"})
	noteTo(m.ownerPlanner(s.path), roleNote, "Ran `"+a.text+"` for "+s.name+".")
	o.detail = "grove runs it in the task's worktree and gives the agent the output"
	return tea.Batch(m.logApproval(s, a, "approve", byPlanner, control.Applied, why), m.runCard(s, a.text)), o, false
}

// cardRanMsg is how a command card the planner approved went.
type cardRanMsg struct {
	path, line string
	out        string
	code       int
	err        error
}

// runCard runs line in s's worktree in the background: plain git as grove
// runs the agent's routine git, anything else through the shell, both
// under the git guard.
func (m *Model) runCard(s *session, line string) tea.Cmd {
	m.auto.running[s]++ // the task doesn't count as finished meanwhile
	dir, env, exec := s.path, m.guardEnv(), m.auto.exec
	ctx := m.ctx
	return func() tea.Msg {
		msg := cardRanMsg{path: dir, line: line}
		if args, ok := plainGit(line); ok {
			c, cancel := context.WithTimeout(ctx, gitTimeout)
			b, code, err := exec(c, dir, args, env)
			cancel()
			msg.out, msg.code, msg.err = string(b), code, err
			return msg
		}
		c, cancel := context.WithTimeout(ctx, cardTimeout)
		defer cancel()
		r := shell.RunEnv(c, dir, line, 8000, env)
		msg.out, msg.code, msg.err = string(r.Output), r.ExitCode, r.Err
		if r.Err == nil && r.ExitCode != 0 {
			msg.err = errExit
		}
		return msg
	}
}

// errExit stands for a command's non-zero exit.
var errExit = exitError{}

type exitError struct{}

func (exitError) Error() string { return "non-zero exit" }

func (m *Model) onCardRan(msg cardRanMsg) tea.Cmd {
	s := m.sessionFor(msg.path)
	if s == nil {
		return m.startScan()
	}
	if m.auto.running[s]--; m.auto.running[s] <= 0 {
		delete(m.auto.running, s)
	}
	status := "exit 0"
	if msg.err != nil {
		status = msg.err.Error()
		if msg.code > 0 {
			status = "exit " + strconv.Itoa(msg.code)
		}
	}
	note := "$ " + msg.line + "  (" + status + ")"
	if out := strings.TrimSpace(tail(msg.out, 3000)); out != "" {
		note += "\n\n```\n" + out + "\n```"
	}
	role := roleNote
	if msg.err != nil {
		role = roleError
	}
	s.entries = append(s.entries, chatEntry{role, note})
	text := "The planner approved the command you asked for, and grove ran it in your worktree:\n\n" + note + "\n\nCarry on with your task."
	cmds := []tea.Cmd{m.startScan()}
	if len(s.pending) > 0 || s.running {
		s.notes = append(s.notes, text)
		return tea.Batch(cmds...)
	}
	return tea.Batch(append(cmds, m.sendAs(s, usage.KindResume, s.withQueued(text)))...)
}
