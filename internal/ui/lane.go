package ui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/usage"
)

// A planner busy with a long turn (reading diffs, running tests) would keep
// its agents waiting on approvals and questions. Once it has been working
// for laneWait, those go to its approval lane instead: a second planner
// session for the same owner (lead or group), in m.lanes under the owner's
// crewKey, with its own Claude process (Slot "<crewKey>-lane"). It gets only
// those items, through the same orchPrompt and rules, and its reply goes
// through applyDecisions (so a group's lane keeps to its group, having the
// same crewPlan and crewGroup) and leaveUndecided. At most one lane turn
// runs per owner. The lane has no chat of its own and isn't saved: its
// owner's chat gets a line about what it answered, and its usage counts for
// the same plan and group as decide turns.

// laneWait is how long an owner must have been working before its
// approvals and questions go to its lane.
const laneWait = 30 * time.Second

// slot is s's agent.Request.Slot: a separate Claude process for each
// planner and each lane.
func (s *session) slot() string {
	if s.lane {
		return s.crewKey() + "-lane"
	}
	return s.crewKey()
}

// laneOwner is the planner a lane works for.
func (m *Model) laneOwner(lane *session) *session {
	if lane.crewGroup == "" {
		return m.planner
	}
	return m.crews[lane.crewKey()]
}

// orchestrateLane starts a lane turn for the urgent approvals and questions
// among items, all of one owner, when that owner has been busy for
// laneWait. It returns the items it didn't take.
func (m *Model) orchestrateLane(items []orchItem) (tea.Cmd, []orchItem) {
	planID, groupID := items[0].plan, m.ownerOf(items[0])
	owner := m.planner
	if groupID != "" {
		owner = m.crews[crewKey(planID, groupID)]
	}
	if owner == nil || !owner.running || owner.started.IsZero() || m.now.Sub(owner.started) < laneWait {
		return nil, items
	}
	var take, keep []orchItem
	for _, it := range items {
		if it.urgent && (it.kind == orchApproval || it.kind == orchQuestion) {
			take = append(take, it)
		} else {
			keep = append(keep, it)
		}
	}
	if len(take) == 0 || !m.itemsSettled(take) {
		return nil, items
	}
	key := owner.crewKey()
	lane := m.lanes[key]
	if lane != nil && lane.running {
		return nil, items
	}
	ownerSt := m.stateOf(owner)
	if ownerSt.turns >= m.cfg.Planner.Decisions() {
		return nil, items // the owner's own turn says it paused
	}
	if lane == nil {
		lane = m.newPlanner(owner.path, owner.kind)
		lane.lane, lane.model = true, owner.model
		lane.crewPlan, lane.crewGroup = owner.crewPlan, owner.crewGroup
		lane.name = owner.name + " · lane"
		m.lanes[key] = lane
	}
	st := m.stateOf(lane)
	if m.now.Before(st.retryAt) {
		return nil, items
	}
	ownerSt.turns++
	text, summary := m.orchPrompt(lane, take)
	for _, it := range take {
		st.asked = append(st.asked, it.ids...)
	}
	lane.nextPlan = planIDs(take)[0]
	lane.nextKind = usage.KindDecide
	st.reports = nil
	st.sent = slices.Clone(take)
	m.markSent(take)
	rules, _ := m.rulesFor(lane)
	cmd := m.send(lane, text)
	lane.rulesSent = rulesKey(rules)
	lane.entries[len(lane.entries)-1] = chatEntry{roleNote, summary}
	return cmd, keep
}

// laneDone tells the lane's owner what the lane answered for it (ok: the
// turn didn't fail), and passes on decisions that didn't apply.
func (m *Model) laneDone(lane *session, ok bool) tea.Cmd {
	owner := m.laneOwner(lane)
	st := m.stateOf(lane)
	if ownerSt := m.stateOf(owner); owner != nil && len(st.reports) > 0 {
		ownerSt.reports = append(ownerSt.reports, st.reports...)
		st.reports = nil
	}
	if !ok {
		return nil
	}
	var done []string
	n := 0
	for _, id := range st.asked {
		for _, s := range m.allSessions() {
			for _, r := range s.answered {
				if r.ID == id && r.By == byPlanner {
					verb := "denied"
					if r.Approve {
						verb = "approved"
					}
					done = append(done, fmt.Sprintf("%s `%s` for %s", verb, firstLine(r.Text), s.name))
					n++
				}
			}
		}
	}
	for _, it := range st.sent {
		if it.kind == orchQuestion {
			done = append(done, "answered "+it.branch+"'s question")
		}
	}
	if len(done) == 0 {
		return nil
	}
	text := "Answered " + plural(n, "approval") + " while busy: " + strings.Join(done, "; ")
	if n == 0 {
		text = "While busy: " + strings.Join(done, "; ")
	}
	noteTo(owner, roleNote, text)
	return nil
}
