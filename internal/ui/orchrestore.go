package ui

import (
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// The planners' queue isn't saved: after a restart it is rebuilt from what
// is saved (rebuildOrch), once both the sessions and the plans have loaded.
// Each watched task's agent that isn't working is handed to queueTurn again
// (approvals that wait for a planner, a closing question, failed, finished
// and idle tasks), and each running plan whose tasks are all done is queued
// to merge. Chat messages are found again through the planners' read
// positions (orchestrator.told), which are saved in orchFile and move only
// when a planner is sent them, so messages posted while grove was closed,
// or queued but not sent, still reach the planner. News nothing else
// records (how a merge or its tests went, review comments, a check a
// planner asked for) is saved in orchFile itself while it waits or is in a
// planner's turn (savedItems), and queued again when the plans load.

// orchFile holds the planners' read positions in the chats, and the news
// they haven't had yet.
const orchFile = "planner/orch.json"

// savedOrch is orchFile's content.
type savedOrch struct {
	Told    map[string]int `json:"told,omitempty"`    // by chat folder
	Pending []savedItem    `json:"pending,omitempty"` // news no planner had when grove closed
}

// savedItem is a queued item whose news is saved nowhere else.
type savedItem struct {
	Kind   string   `json:"kind"` // merge, review or check
	Plan   string   `json:"plan"`
	Branch string   `json:"branch,omitempty"`
	Group  string   `json:"group,omitempty"`
	Lines  []string `json:"lines"`
}

// savedKinds are the items savedItems keeps, by their names in orchFile.
var savedKinds = map[orchKind]string{orchMerge: "merge", orchReview: "review", orchCheck: "check"}

// savedItems is the news to keep for after a restart: queued, or sent to a
// planner whose turn hasn't ended, which a restart cuts short.
func (m *Model) savedItems() []savedItem {
	items := slices.Clone(m.orch.queue)
	items = append(items, m.orch.sent...)
	for _, s := range m.allSessions() {
		if s.planner && (s.crewGroup != "" || s.side) {
			items = append(items, s.crew.sent...)
		}
	}
	var out []savedItem
	for _, it := range items {
		if kind, ok := savedKinds[it.kind]; ok {
			out = append(out, savedItem{Kind: kind, Plan: it.plan, Branch: it.branch, Group: it.group, Lines: it.lines})
		}
	}
	return out
}

// restoreOrch takes the saved read positions, unless the chat was read
// already.
func (m *Model) restoreOrch(v savedOrch) {
	if m.orch.seen == nil {
		m.orch.seen = map[string]int{}
	}
	if m.orch.told == nil {
		m.orch.told = map[string]int{}
	}
	for dir, n := range v.Told {
		if _, ok := m.orch.seen[dir]; !ok {
			m.orch.seen[dir], m.orch.told[dir] = n, n
		}
	}
	for _, it := range v.Pending {
		for kind, name := range savedKinds {
			if name == it.Kind {
				m.orch.queue = append(m.orch.queue, orchItem{kind: kind, plan: it.Plan, branch: it.Branch, group: it.Group, lines: it.Lines, urgent: true})
			}
		}
	}
	m.orch.savedPending = len(v.Pending) > 0
}

// savedOrchState is what orchFile should hold, and whether to write it:
// only once the saved file was read, so it is never overwritten with less.
// Once news was saved, the file is kept up to date until it is empty.
func (m *Model) savedOrchState() (savedOrch, bool) {
	v := savedOrch{Told: maps.Clone(m.orch.told), Pending: m.savedItems()}
	m.orch.savedPending = m.orch.savedPending || len(v.Pending) > 0
	return v, m.plansLoaded && (len(v.Told) > 0 || m.orch.savedPending)
}

// markSent notes that a planner was asked about items: chats' read
// positions and finished tasks' HEADs move on.
func (m *Model) markSent(items []orchItem) {
	for _, it := range items {
		switch it.kind {
		case orchGroup:
			if it.dir != "" && it.upto > m.orch.told[it.dir] {
				if m.orch.told == nil {
					m.orch.told = map[string]int{}
				}
				m.orch.told[it.dir] = it.upto
			}
		case orchFinished:
			if _, _, s := m.watchedTask(it.branch); s != nil && it.head != "" {
				s.finishedHead = it.head
			}
		}
	}
}

// afterLoad rebuilds the planners' queue once the saved sessions and plans
// have both loaded, in either order.
func (m *Model) afterLoad() tea.Cmd {
	if !m.sessionsLoaded || !m.plansLoaded || m.rebuilt {
		return nil
	}
	m.rebuilt = true
	return m.rebuildOrch()
}

// rebuildOrch queues again what waited for the planners when grove last
// closed, and sends messages the user queued for agents that are free.
func (m *Model) rebuildOrch() tea.Cmd {
	var cmds []tea.Cmd
	for _, p := range m.plans {
		if !watched(p) || p.Status != plan.Running && p.Status != plan.Review {
			continue
		}
		for i := range p.Tasks {
			t := &p.Tasks[i]
			s := m.sessionFor(t.Path)
			if t.Path == "" || s == nil || s.running {
				continue
			}
			if s.lastReply.Len() == 0 {
				s.lastReply.WriteString(lastReplyOf(s))
			}
			// Finished news counts again only if the planner wasn't asked
			// about this HEAD (markSent).
			cmds = append(cmds, m.queueTurn(s, true))
		}
		cmds = append(cmds, m.finishIfSettled(p))
	}
	for _, s := range m.allSessions() {
		cmds = append(cmds, m.sendQueued(s))
	}
	return tea.Batch(append(cmds, m.orchestrate())...)
}

// lastReplyOf is the agent's reply to the last message in its chat, as
// lastReply held it before grove closed.
func lastReplyOf(s *session) string {
	var parts []string
	for i := len(s.entries) - 1; i >= 0; i-- {
		e := s.entries[i]
		if e.role == roleUser {
			break
		}
		if e.role == roleAgent {
			parts = append([]string{e.text}, parts...)
		}
	}
	return strings.Join(parts, "\n\n")
}
