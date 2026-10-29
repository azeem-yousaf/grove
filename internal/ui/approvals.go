package ui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/git"
)

// Approvals: a command a task's agent was refused becomes a pendingAction
// with a stable ID and what grove's git policy says about it (whether only
// the user may answer). The user answers on the card; for a plan the planner
// watches, the planner may answer the rest, by ID or by command. Every
// answer goes through answer, which records it so a repeat changes nothing,
// lets the command through the agent's hook (m.grant) as well as Claude's
// permission rules, logs it to approvals.jsonl and notes it in the chats.
// resume then carries the agent on: after the planner's answers as soon as
// nothing is left for the planner, telling it what still waits for the
// user; after the user's once every card is answered.

// Who answered an approval, as approvals.jsonl records it.
const (
	byUser    = "user"
	byPlanner = "planner"
	byPolicy  = "grove-policy"
)

// answerRec is an approval that was answered.
type answerRec struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Approve bool   `json:"approve,omitempty"`
	By      string `json:"by"`
}

// keepAnswers is how many answers a session remembers.
const keepAnswers = 50

// nextApprovalID is unique among the repository's agents: the worktree's
// key and a count saved with the session.
func (s *session) nextApprovalID() string {
	s.seq++
	return git.PathKey(s.path)[:6] + "-" + strconv.Itoa(s.seq)
}

// newApproval makes the pending approval for a command s was refused.
func (m *Model) newApproval(s *session, tool, command string) pendingAction {
	return pendingAction{
		kind:   pendingApproval,
		id:     s.nextApprovalID(),
		text:   command,
		tool:   tool,
		policy: m.classify(s, tool, command),
	}
}

// awaitsPlanner reports whether the planner is still to answer a.
func (m *Model) awaitsPlanner(s *session, a pendingAction) bool {
	return a.kind == pendingApproval && !a.policy.userOnly && !a.left && m.watchedBy(s.path)
}

// answer approves or denies s.pending[i] on behalf of by.
func (m *Model) answer(s *session, i int, approve bool, by, why string) tea.Cmd {
	return m.answerWith(s, i, approve, by, why, "")
}

// answerWith is answer, approving with rule (familyRule's) in place of the
// exact command's when it is set.
func (m *Model) answerWith(s *session, i int, approve bool, by, why, rule string) tea.Cmd {
	a := s.pending[i]
	s.pending = slices.Delete(s.pending, i, i+1)
	why = strings.TrimSpace(why)
	s.answered = append(s.answered, answerRec{ID: a.id, Text: a.text, Approve: approve, By: by})
	if n := len(s.answered); n > keepAnswers {
		s.answered = slices.Clone(s.answered[n-keepAnswers:])
	}
	if by == byPlanner {
		s.byPlanner = true
	}
	who := ""
	if by == byPlanner {
		who = " by the planner"
	}
	var cmds []tea.Cmd
	verdict, done := "deny", "denied"
	if approve {
		verdict, done = "approve", "approved"
		note := "Allowed" + who + ": " + a.text
		if rule == "" {
			rule = agent.ClaudeRule(a.tool, a.text)
		} else {
			note += " (and the rest of its family: " + rule + ")"
		}
		s.allow = append(s.allow, rule)
		s.approved = append(s.approved, a.text)
		s.entries = append(s.entries, chatEntry{roleNote, note})
		// Called right away: the agent may be resumed before a command runs.
		if err := m.grant(s, a.tool, a.text, a.policy, by); err != nil {
			cmds = append(cmds, m.grantFailed(s, a.text, err))
		}
	} else {
		note := "Denied" + who + ": " + a.text
		if why != "" {
			note += " (" + why + ")"
		}
		s.entries = append(s.entries, chatEntry{roleNote, note})
		if by == byPlanner {
			item := "`" + a.text + "`"
			if why != "" {
				item += " (" + why + ")"
			}
			s.denied = append(s.denied, item)
			// The planner is in charge; the user can see it in the chat.
			cmds = append(cmds, m.notify("The planner denied "+s.name+": "+firstLine(a.text), toastInfo))
		}
	}
	if m.watchedBy(s.path) && by != byPlanner {
		noteTo(m.ownerPlanner(s.path), roleNote, fmt.Sprintf("The user %s %s's command: %s", done, s.name, a.text))
	} else if by == byPlanner {
		noteTo(m.ownerPlanner(s.path), roleNote, fmt.Sprintf("%s %s for %s.", upperFirst(done), a.text, s.name))
	}
	cmds = append(cmds, m.logApproval(s, a, verdict, by, control.Applied, why))
	return tea.Batch(cmds...)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// refuse turns down the planner's approval of a command only the user may
// answer: it stays for them.
func (m *Model) refuse(s *session, a pendingAction) tea.Cmd {
	why := "needs you"
	if a.policy.reason != "" {
		why += ": " + a.policy.reason
	}
	s.entries = append(s.entries, chatEntry{roleNote, "The planner can't approve this, so it waits for you (" + why + "): " + a.text})
	return tea.Batch(
		m.notify("✦ "+s.name+" "+why, toastInfo),
		m.alert(s.name+" "+why+": "+firstLine(a.text)),
		m.logApproval(s, a, "approve", byPolicy, control.NeedsUser, a.policy.reason))
}

// logApproval appends one answer to approvals.jsonl.
func (m *Model) logApproval(s *session, a pendingAction, verdict, by, outcome, reason string) tea.Cmd {
	if m.store == nil {
		return nil
	}
	e := control.Activity{Time: time.Now(), ID: a.id, Task: s.name, Command: a.text, Verdict: verdict, By: by, Outcome: outcome, Reason: reason}
	if p, t := m.taskFor(s.path); t != nil {
		e.Plan, e.Task = p.ID, t.Branch
	}
	if e.Reason == "" && verdict == "approve" {
		e.Reason = a.policy.reason
	}
	dir := m.store.Dir()
	return func() tea.Msg {
		if err := control.LogActivity(dir, []control.Activity{e}); err != nil {
			return actionMsg{err: fmt.Errorf("couldn't log the approval: %w", err)}
		}
		return nil
	}
}

// grantFailed tells the user an approval may not reach the agent's hook.
func (m *Model) grantFailed(s *session, command string, err error) tea.Cmd {
	text := "Couldn't let the approved command through: " + err.Error()
	s.entries = append(s.entries, chatEntry{roleError, text + " (" + command + "). If it is refused again, approve it again."})
	return tea.Batch(m.notify(text, toastErr), m.alert(text))
}

// resume carries the agent on with the answers it has had since its turn
// ended. After the planner's, it goes on once nothing is left for the
// planner, and is told what still waits for the user; the user answers the
// cards in a row, so after theirs it waits for every card.
func (m *Model) resume(s *session) tea.Cmd {
	if s.running || len(s.approved)+len(s.denied) == 0 {
		return nil
	}
	var waiting []string
	for _, a := range s.pending {
		if a.kind != pendingApproval {
			continue
		}
		if m.awaitsPlanner(s, a) {
			return nil
		}
		waiting = append(waiting, a.text)
	}
	if !s.byPlanner && len(waiting) > 0 {
		return nil
	}
	approved, denied, planner := s.approved, s.denied, s.byPlanner
	s.approved, s.denied, s.byPlanner = nil, nil, false
	if !planner && len(denied) == 0 && len(waiting) == 0 {
		return m.send(s, "I approved `"+strings.Join(approved, "`, `")+"`. Run it now and carry on.")
	}
	var parts []string
	if len(approved) > 0 {
		parts = append(parts, "Approved: `"+strings.Join(approved, "`, `")+"`. Run it now.")
	}
	if len(denied) > 0 {
		parts = append(parts, "Denied: "+strings.Join(denied, ", ")+". Do without it or find another way.")
	}
	if len(waiting) > 0 {
		parts = append(parts, "Still waiting for the user: `"+strings.Join(waiting, "`, `")+"`. Don't run it until you hear; get on with what you can.")
	}
	return m.send(s, strings.Join(parts, " ")+" Then carry on with your task.")
}

// findApproval finds the approval an answer names: by ID among every
// watched task's, else by branch and command. Without a pending one it
// returns the earlier answer to it, if there was one.
func (m *Model) findApproval(branch, id, command string) (*session, int, *answerRec) {
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
	match := func(gotID, text string) bool {
		if id != "" {
			return gotID == id
		}
		return command != "" && control.NormCommand(text) == control.NormCommand(command)
	}
	for _, s := range sessions {
		for i, a := range s.pending {
			if a.kind == pendingApproval && match(a.id, a.text) {
				return s, i, nil
			}
		}
	}
	for _, s := range sessions {
		for i := len(s.answered) - 1; i >= 0; i-- {
			if r := &s.answered[i]; match(r.ID, r.Text) {
				return s, -1, r
			}
		}
	}
	return nil, -1, nil
}

// askAgain is how many planner turns may pass over an approval before
// grove denies it for the planner.
const askAgain = 2

// leaveUndecided deals with what the planner was asked about in the turn
// that just ended and didn't answer, so nothing waits on a planner that
// has moved on. The planner is in charge of a plan it watches, so the user
// never gets those: it is asked again, and after askAgain turns without an
// answer the command is denied and the agent told to find another way.
// Approvals of plans it doesn't watch any more go to the user.
func (m *Model) leaveUndecided(planner *session) tea.Cmd {
	st := m.stateOf(planner)
	asked := st.asked
	st.asked = nil
	var cmds []tea.Cmd
	var left, denied []string
	for _, s := range m.allSessions() {
		changed := false
		for i := 0; i < len(s.pending); i++ {
			a := &s.pending[i]
			if a.kind != pendingApproval || !slices.Contains(asked, a.id) || a.left {
				continue
			}
			if m.awaitsPlanner(s, *a) {
				if a.asked++; a.asked < askAgain {
					st.reports = append(st.reports, fmt.Sprintf("You didn't answer %s's `%s` (id %s). Nobody else will: approve or deny it now.", s.name, firstLine(a.text), a.id))
					m.queueTurn(s)
					continue
				}
				denied = append(denied, s.name+": "+firstLine(a.text))
				cmds = append(cmds, m.answer(s, i, false, byPlanner, "the planner didn't approve it; do without it, or explain to the planner why it is needed"))
				i--
				changed = true
				continue
			}
			a.left, changed = true, true
			s.entries = append(s.entries, chatEntry{roleNote, "The planner left this for you: " + a.text})
			left = append(left, s.name+": "+firstLine(a.text))
		}
		if changed {
			cmds = append(cmds, m.resume(s))
		}
	}
	if len(denied) > 0 {
		noteTo(planner, roleNote, "Denied as you didn't answer: "+strings.Join(denied, "; "))
		st.reports = append(st.reports, "You didn't answer, so grove denied: "+strings.Join(denied, "; ")+". Answer every approval you are asked about.")
	}
	if len(left) > 0 {
		noteTo(planner, roleNote, "Left for the user: "+strings.Join(left, "; "))
		text := plural(len(left), "command") + " the planner left for you: " + strings.Join(left, "; ")
		cmds = append(cmds, m.notify("✦ "+text, toastInfo), m.alert(text))
	}
	return tea.Batch(cmds...)
}
