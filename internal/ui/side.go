package ui

import (
	"cmp"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/usage"
)

// A planner's turn can take many minutes, for example while it runs a test
// suite, and its agents' approvals and questions shouldn't wait for it. While
// a planner (the lead or a group's) is busy, its urgent approvals and
// questions go to a side turn: a fork of its conversation
// (agent.Request.Fork), so it knows everything the planner knew when the
// turn started, in a session of its own kept in m.sides. At most one runs
// per planner. Its block is scoped like its owner's and can't merge or
// close; its decisions take the same paths as the planner's, so one the
// planner makes again later is "already applied". Its turns count towards
// its owner's decision limit, its usage goes in the ledger under its own
// agent, and what it decided is noted in the owner's chat and told in the
// owner's next request (whileBusy). It is never shown; it is saved only so
// its usage survives a restart.

// sideKey is the side turn's key in m.sides for the planner with crewKey
// owner, and its Claude process's Slot.
func sideKey(owner string) string { return owner + "\x00side" }

// slot keeps s's Claude process apart from other planners' in its folder.
func (s *session) slot() string {
	if s.side {
		return sideKey(s.crewKey())
	}
	return s.crewKey()
}

// sideAsk ends a side turn's request.
const sideAsk = "This is a side turn: a fork of your conversation, made because your main turn is still busy. " +
	"Answer only these, quickly. Don't run long commands such as test suites or builds, and don't message agents or post in chats " +
	"unless an answer needs it. grove's tools aren't available here: answer in the grove-orchestrate block. " +
	"Merging and closing wait for your main turn, which hears what you decided here."

// sideItem reports whether it may be answered in a side turn.
func sideItem(it orchItem) bool {
	return it.urgent && (it.kind == orchApproval || it.kind == orchQuestion)
}

// sideTurn answers owner's urgent approvals and questions among items in a
// side turn while owner is busy, returning the items still waiting. Nothing
// is forked before owner has a conversation, and nothing runs past its
// decision limit.
func (m *Model) sideTurn(owner *session, items []orchItem) (tea.Cmd, []orchItem) {
	var take, rest []orchItem
	for _, it := range items {
		if sideItem(it) {
			take = append(take, it)
		} else {
			rest = append(rest, it)
		}
	}
	key := sideKey(owner.crewKey())
	side, st := m.sides[key], m.stateOf(owner)
	if len(take) == 0 || owner.id == "" || side != nil && side.running || st.turns >= m.cfg.Planner.Decisions() {
		return nil, items
	}
	if side == nil {
		side = &session{side: true, planner: true, touched: map[string]bool{}, seen: true, uid: newUID(), created: time.Now()}
		m.sides[key] = side
	}
	// Each side turn forks the owner's conversation as it is now.
	side.sideOf, side.name = owner, owner.name+" (side turn)"
	side.kind, side.model, side.path = owner.kind, owner.model, owner.path
	side.crewPlan, side.crewGroup = owner.crewPlan, owner.crewGroup
	side.allow, side.history, side.entries = slices.Clone(owner.allow), slices.Clone(owner.history), nil
	side.setSessionID(owner.id)
	side.sessionTotal = owner.sessionTotal
	// The fork has the rules its owner's conversation has, so it gets the
	// short line whenever its owner would. Its rules differ only by the
	// control tools, which sideAsk says it doesn't have.
	side.rulesSent = ""
	if _, full := m.rulesFor(owner); !full {
		side.rulesSent = rulesKey(m.orchRules(side))
	}
	st.turns++

	own := m.stateOf(side)
	text, summary := m.orchPrompt(side, take)
	own.asked, own.reports = nil, nil
	for _, it := range take {
		own.asked = append(own.asked, it.ids...)
	}
	side.nextPlan = take[0].plan
	cmd := m.sendAs(side, usage.KindDecide, text+"\n"+sideAsk+"\n")
	noteTo(owner, roleNote, "While you work, a side turn decides: "+strings.TrimPrefix(summary, "Asked to decide: "))
	return cmd, rest
}

// onSideDone carries out what a side turn decided.
func (m *Model) onSideDone(s *session, ev agent.Event, reply string, recorded tea.Cmd) tea.Cmd {
	if ev.Err != nil {
		noteTo(s, roleError, "Side turn: "+ev.Err.Error())
	}
	return tea.Batch(recorded, m.applyDecisions(s, reply), m.leaveUndecided(s), m.orchestrate())
}

// scopeSide keeps merging and closing from a side turn: they wait for its
// owner's main turn. Other sessions' decisions pass unchanged.
func scopeSide(s *session, d decisions) (decisions, []outcome) {
	if s == nil || !s.side {
		return d, nil
	}
	var outs []outcome
	for _, mg := range d.Merge {
		outs = append(outs, outcome{what: "merge " + mg.Plan, result: control.Failed, detail: "a side turn only answers approvals and questions; merge in your main turn"})
	}
	for _, c := range d.Close {
		outs = append(outs, outcome{what: "close " + c.Plan, result: control.Failed, detail: "a side turn only answers approvals and questions; close in your main turn"})
	}
	d.Merge, d.Close = nil, nil
	return d, outs
}

// sideAccount describes a side turn's decisions for its owner, in the order
// decide reports their outcomes.
func (m *Model) sideAccount(d decisions) []string {
	name := func(a answerDecision) (string, string) {
		if s, i, rec := m.findApproval(a.Branch, a.ID, a.Command); s != nil && rec == nil {
			return "`" + firstLine(s.pending[i].text) + "`", s.name
		}
		if a.Command != "" {
			return "`" + a.Command + "`", a.Branch
		}
		return a.ID, a.Branch
	}
	var out []string
	for _, a := range d.Approve {
		c, b := name(a)
		out = append(out, "approved "+c+" for "+b)
	}
	for _, a := range d.Deny {
		c, b := name(a)
		t := "denied " + c + " for " + b
		if a.Reason != "" {
			t += " (" + a.Reason + ")"
		}
		out = append(out, t)
	}
	for _, r := range d.Reply {
		out = append(out, "answered "+r.Branch+": "+clipText(r.Message, 300))
	}
	for _, mk := range d.Mark {
		out = append(out, "marked "+mk.Branch+" "+mk.Status)
	}
	for _, p := range d.Post {
		out = append(out, "posted in "+cmp.Or(p.Group, plan.ChatID)+": "+clipText(p.Message, 200))
	}
	return out
}

// tellOwner passes what a side turn s decided, told by sideAccount, to its
// owner: for its next request, and as notes in its chat. Answers to
// approvals are already noted there by answer.
func (m *Model) tellOwner(s *session, told []string, outs []outcome) {
	owner := s.sideOf
	if !s.side || owner == nil {
		return
	}
	st := m.stateOf(owner)
	for i, o := range outs {
		if i >= len(told) || o.result != control.Applied {
			continue
		}
		st.whileBusy = append(st.whileBusy, told[i])
		if !strings.HasPrefix(told[i], "approved ") && !strings.HasPrefix(told[i], "denied ") {
			noteTo(owner, roleNote, "Side turn: "+told[i])
		}
	}
}

// clipText is s on one line, at most n characters.
func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
