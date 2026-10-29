package ui

import (
	"encoding/json"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// While a plan the planner watches is running, the planner may also run
// shell commands without asking (still no edits of its own), for example to
// check a task's diff and tests, and it gets grove's control tools (package
// control): see the tasks, message their agents, answer their approval
// requests and mark tasks done or failed. grove publishes the watched tasks
// to the control folder and carries out the orders the tools queue there
// every second, through the same decide as grove-orchestrate blocks.

type controlMsg struct {
	orders []control.Order
	err    error
}

// orchestrating reports whether a plan the planner watches is running.
func (m *Model) orchestrating() bool {
	for _, p := range m.plans {
		if watched(p) && p.Status == plan.Running {
			return true
		}
	}
	return false
}

func (m *Model) controlDir() (string, bool) {
	if m.store == nil {
		return "", false
	}
	return control.Dir(m.store.Dir()), true
}

// controlTools gives the planner its control tools while it watches a
// running plan.
func (m *Model) controlTools(s *session) []agent.ToolServer {
	dir, ok := m.controlDir()
	if !s.planner || !ok || m.groveBin == "" || !m.orchestrating() {
		return nil
	}
	return []agent.ToolServer{{
		Name:    "grove",
		Command: m.groveBin,
		Args:    []string{"mcp-plan", "--dir", dir},
		Allow:   control.Tools,
	}}
}

// controlState describes the watched running plans for the tools.
func (m *Model) controlState() control.State {
	var st control.State
	for _, p := range m.plans {
		if !watched(p) || p.Status != plan.Running {
			continue
		}
		cp := control.Plan{Title: p.Title, Status: string(p.Status)}
		for _, t := range p.Tasks {
			ct := control.Task{Branch: t.Branch, Title: t.Title, Group: t.Group, Status: string(t.Status), Error: t.Error, Path: t.Path}
			if s := m.sessionFor(t.Path); t.Path != "" && s != nil {
				ct.Busy = s.running
				ct.LastReply = tail(lastReply(s), 1500)
				for _, a := range s.pending {
					if a.kind == pendingApproval {
						ct.Approvals = append(ct.Approvals, control.Approval{Tool: a.tool, Command: a.text})
					}
				}
			}
			cp.Tasks = append(cp.Tasks, ct)
		}
		st.Plans = append(st.Plans, cp)
	}
	return st
}

// lastReply is the agent's latest reply, even from before grove restarted.
func lastReply(s *session) string {
	if r := s.lastReply.String(); r != "" {
		return r
	}
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].role == roleAgent {
			return s.entries[i].text
		}
	}
	return ""
}

// tail is the end of text, at most n bytes, cut at a line where it can be.
func tail(text string, n int) string {
	text = strings.TrimSpace(text)
	if len(text) <= n {
		return text
	}
	text = text[len(text)-n:]
	if i := strings.IndexByte(text, '\n'); i >= 0 && i < n/2 {
		text = text[i+1:]
	}
	return "…" + text
}

// pollControl publishes the watched plans when they changed and collects
// the planner's orders.
func (m *Model) pollControl() tea.Cmd {
	dir, ok := m.controlDir()
	if !ok || m.pollingControl {
		return nil
	}
	state := m.controlState()
	b, err := json.Marshal(state)
	if err != nil {
		return nil
	}
	write := string(b) != string(m.controlJSON)
	if !write && !m.orchestrating() {
		return nil
	}
	m.pollingControl = true
	if write {
		m.controlJSON = b
	}
	return func() tea.Msg {
		if write {
			if err := control.WriteState(dir, state); err != nil {
				return controlMsg{err: err}
			}
		}
		orders, err := control.Take(dir)
		return controlMsg{orders: orders, err: err}
	}
}

func (m *Model) onControl(msg controlMsg) tea.Cmd {
	m.pollingControl = false
	if msg.err != nil {
		m.controlJSON = nil // write it again next time
	}
	if len(msg.orders) == 0 {
		return nil
	}
	var d decisions
	for _, o := range msg.orders {
		switch o.Action {
		case control.Message:
			d.Reply = append(d.Reply, struct{ Branch, Message string }{o.Task, o.Text})
		case control.Approve:
			d.Approve = append(d.Approve, struct{ Branch, Command string }{o.Task, o.Text})
		case control.Deny:
			d.Deny = append(d.Deny, struct{ Branch, Command, Reason string }{o.Task, o.Text, o.Note})
		case control.Finish:
			d.Mark = append(d.Mark, struct{ Branch, Status, Note string }{o.Task, "finished", o.Note})
		case control.Fail:
			d.Mark = append(d.Mark, struct{ Branch, Status, Note string }{o.Task, "failed", o.Note})
		}
	}
	return m.decide(d)
}
