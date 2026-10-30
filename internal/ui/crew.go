package ui

import (
	"cmp"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// Every group of a plan the planner watches has its own planner, its crew
// planner, which owns that group: it answers the group's agents' approvals
// and questions, takes on the ones that struggle, checks their finished
// work and settles their group chat. The lead planner (m.planner) keeps
// the ungrouped tasks, the plan chat, merging and the pull request. A crew
// planner is a planner session with crewPlan and crewGroup set, kept in
// m.crews and saved under crewDir; it runs in the planner's folder with a
// Claude process of its own (agent.Request.Slot), and the user can open it
// (ctrl+p in its group chat, P on its group in the list) and talk to it.

// plannerState is a planner's state as an orchestrator: the lead's in
// m.orch, each crew planner's in its session.
type plannerState struct {
	turns   int      // turns in a row without the user or progress
	paused  bool     // stopped at the limit until the user says something
	asked   []string // approvals in its current turn, by ID
	reports []string // its decisions that didn't apply, for its next request
}

// crewKey identifies a group's planner; "" for the lead.
func crewKey(planID, groupID string) string {
	if groupID == "" {
		return ""
	}
	return planID + "\x00" + groupID
}

func (s *session) crewKey() string { return crewKey(s.crewPlan, s.crewGroup) }

// stateOf is s's state as an orchestrator.
func (m *Model) stateOf(s *session) *plannerState {
	if s != nil && s.crewGroup != "" {
		return &s.crew
	}
	return &m.orch.plannerState
}

// planners is every planner: the lead, if there is one, and the crews.
func (m *Model) planners() []*session {
	var out []*session
	if m.planner != nil {
		out = append(out, m.planner)
	}
	for _, s := range m.crews {
		out = append(out, s)
	}
	return out
}

// ownerOf is the group whose planner handles it, "" for the lead: a group
// task's news, and its group chat's messages.
func (m *Model) ownerOf(it orchItem) string {
	switch it.kind {
	case orchApproval, orchQuestion, orchFinished, orchFailed, orchIdle:
		if _, t, _ := m.watchedTask(it.branch); t != nil {
			return t.Group
		}
	case orchGroup:
		if it.group != plan.ChatID {
			return it.group
		}
	}
	return ""
}

// plannerFor is the planner for group of planID, made if needed: the
// lead's for "". nil when there is no folder to run it in.
func (m *Model) plannerFor(planID, groupID string) *session {
	if groupID == "" {
		if m.planner == nil {
			dir := m.plannerDir()
			k, ok := agent.Parse(m.planByID(planID).Agent)
			if dir == "" || !ok {
				return nil
			}
			m.planner = m.newPlanner(dir, k)
		}
		return m.planner
	}
	if s := m.crews[crewKey(planID, groupID)]; s != nil {
		return s
	}
	p := m.planByID(planID)
	g, ok := p.Group(groupID)
	dir := m.plannerDir()
	k, kok := agent.Parse(p.Agent)
	if !ok || dir == "" || !kok {
		return nil
	}
	s := m.newPlanner(dir, k)
	s.name = "planner · " + g.Name
	s.crewPlan, s.crewGroup = planID, groupID
	m.crews[s.crewKey()] = s
	return s
}

// openCrew shows a group's planner, made if needed. Only plans the planner
// watches have them.
func (m *Model) openCrew(planID, groupID string) tea.Cmd {
	p := m.planByID(planID)
	if !watched(p) || groupID == "" || groupID == plan.ChatID {
		return m.notify("A group has its own planner only while the planner is in charge of the plan", toastInfo)
	}
	s := m.plannerFor(planID, groupID)
	if s == nil {
		return m.notify("The planner needs a worktree with files to read", toastInfo)
	}
	return m.showChat(s)
}

// crewContext briefs a group's planner.
func (m *Model) crewContext(s *session) string {
	p := m.planByID(s.crewPlan)
	if p == nil {
		return m.plannerContext()
	}
	g, ok := p.Group(s.crewGroup)
	if !ok {
		return m.plannerContext()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# You are the planner in charge of the group %q of the plan %q\n\n", g.Name, p.Title)
	b.WriteString("grove is a terminal app for git worktrees. AI agents work on this plan's tasks at the same time, each in its own git worktree and branch. " +
		"The user put the planner in charge and only reviews the pull request at the end, so you own this group: nobody else answers its agents.\n\n")
	b.WriteString("Your group's tasks:\n\n")
	for _, t := range p.Tasks {
		if t.Group != s.crewGroup {
			continue
		}
		fmt.Fprintf(&b, "- %s: branch %s, worktree %s\n  Brief: %s\n", t.Title, t.Branch, cmp.Or(t.Path, "not created yet"), quoteText(t.Brief))
	}
	b.WriteString("\nRules:\n")
	fmt.Fprintf(&b, "- You read the repository in %s and the group's worktrees, and you may run shell commands there for the agents, but you never edit files yourself; the agents do.\n", s.path)
	b.WriteString("- grove asks you when the group's agents need something: commands they were refused, questions, finished or failed work, and the group chat. " +
		"Answer every one: approve or deny commands, answer questions from the plan and the code, check finished work and send back what is missing, " +
		"and help agents that struggle until they can go on.\n")
	b.WriteString("- The lead planner runs the rest of the plan, merges it and opens its pull request; you can't merge or close the plan, and you only act on your group's tasks and its chat.\n")
	b.WriteString(plannerGitRules())
	b.WriteString("- The user may also talk to you here: do what they ask within your group.\n")
	b.WriteString("- Replies appear in a narrow terminal panel: keep prose short.\n")
	return b.String()
}

// inCrew reports whether branch is a task of s's group; the lead may act
// on every watched task.
func (m *Model) inCrew(s *session, branch string) bool {
	if s == nil || s.crewGroup == "" {
		return true
	}
	p, t, _ := m.watchedTask(branch)
	return t != nil && p.ID == s.crewPlan && t.Group == s.crewGroup
}

// scopeDecisions keeps a group planner's decisions to its own group: other
// tasks are out of reach, its posts go to its group chat unless it names
// the plan chat, and merging and closing are the lead's.
func (m *Model) scopeDecisions(s *session, d decisions) (decisions, []outcome) {
	if s == nil || s.crewGroup == "" {
		return d, nil
	}
	var outs []outcome
	out := func(what string) {
		outs = append(outs, outcome{what: what, result: control.NotFound, detail: "not a task of your group"})
	}
	keepAnswers := func(as []answerDecision, verb string) []answerDecision {
		var kept []answerDecision
		for _, a := range as {
			if m.inCrew(s, a.Branch) {
				kept = append(kept, a)
			} else {
				out(verb + " for " + a.Branch)
			}
		}
		return kept
	}
	d.Approve = keepAnswers(d.Approve, "approve")
	d.Deny = keepAnswers(d.Deny, "deny")
	var replies []replyDecision
	for _, r := range d.Reply {
		if m.inCrew(s, r.Branch) {
			replies = append(replies, r)
		} else {
			out("reply to " + r.Branch)
		}
	}
	d.Reply = replies
	var marks []markDecision
	for _, mk := range d.Mark {
		if m.inCrew(s, mk.Branch) {
			marks = append(marks, mk)
		} else {
			out("mark " + mk.Branch)
		}
	}
	d.Mark = marks
	for i := range d.Post {
		d.Post[i].Plan = s.crewPlan
		if d.Post[i].Group != plan.ChatID {
			d.Post[i].Group = s.crewGroup
		}
	}
	for _, mg := range d.Merge {
		outs = append(outs, outcome{what: "merge " + mg.Plan, result: control.Failed, detail: "the lead planner merges the plan"})
	}
	for _, c := range d.Close {
		outs = append(outs, outcome{what: "close " + c.Plan, result: control.Failed, detail: "the lead planner closes the plan"})
	}
	d.Merge, d.Close = nil, nil
	return d, outs
}

// ownerPlanner is the planner in charge of the task in path: its group's,
// once there is one, else the lead.
func (m *Model) ownerPlanner(path string) *session {
	if p, t := m.taskFor(path); t != nil && t.Group != "" {
		if s := m.crews[crewKey(p.ID, t.Group)]; s != nil {
			return s
		}
	}
	return m.planner
}

// noteTo adds a line to planner s's chat.
func noteTo(s *session, role chatRole, text string) {
	if s != nil {
		s.entries = append(s.entries, chatEntry{role, text})
	}
}
