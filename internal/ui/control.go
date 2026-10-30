package ui

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/control"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// While a plan the planner watches is running or in review, the planner may
// also run shell commands without asking (still no edits of its own), for
// example to check a task's diff and tests, and it gets grove's control
// tools (package control): see the tasks, message their agents, answer
// their approval requests, mark tasks done or failed, post in the plans'
// chats, merge a settled plan and close one the user is happy with. grove
// publishes the watched tasks to the control folder and carries out the
// orders the tools queue there every second, through the same decide as
// grove-orchestrate blocks.

type controlMsg struct {
	orders []control.Order
	err    error
}

// orchestrating reports whether a plan the planner watches is running, or
// merged and waiting for the user's feedback.
func (m *Model) orchestrating() bool {
	return slices.ContainsFunc(m.plans, orchestrated)
}

func orchestrated(p *plan.Plan) bool {
	return watched(p) && (p.Status == plan.Running || p.Status == plan.Review)
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
	args := []string{"mcp-plan", "--dir", dir}
	if s.crewGroup != "" {
		// A group's planner may act only on its group.
		args = append(args, "--plan", s.crewPlan, "--group", s.crewGroup)
	}
	return []agent.ToolServer{{
		Name:    "grove",
		Command: m.groveBin,
		Args:    args,
		Allow:   control.Tools,
	}}
}

// controlState describes the watched running plans for the tools.
func (m *Model) controlState() control.State {
	var st control.State
	for _, p := range m.plans {
		if !orchestrated(p) {
			continue
		}
		cp := control.Plan{ID: p.ID, Title: p.Title, Status: string(p.Status), Test: m.testCommand(p)}
		if mg := p.Merge; mg != nil {
			cp.Merge = "branch " + mg.Branch + " in " + mg.Path
			switch {
			case m.testingPlan[p.ID]:
				cp.Merge += "; grove is running the tests"
			case mg.Tests != "":
				cp.Merge += "; the tests " + mg.Tests
			}
			if mg.Fixing != "" {
				cp.Merge += "; " + mg.Fixing + " is resolving a conflict with it"
			}
		}
		for _, g := range p.Groups {
			cp.Groups = append(cp.Groups, g.ID)
		}
		for _, t := range p.Tasks {
			ct := control.Task{Branch: t.Branch, Title: t.Title, Group: t.Group, Status: string(t.Status), Error: t.Error, Path: t.Path}
			if c := m.checkIn(t.Path); c != nil {
				ct.Testing = c.command
			}
			if s := m.sessionFor(t.Path); t.Path != "" && s != nil {
				ct.Busy = s.running
				ct.LastReply = tail(lastReply(s), 1500)
				for _, a := range s.pending {
					if a.kind == pendingApproval {
						ct.Approvals = append(ct.Approvals, control.Approval{ID: a.id, Tool: a.tool, Command: a.text, Reason: a.policy.reason, Planner: !a.policy.userOnly})
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
	if m.orch.orders == nil {
		m.orch.orders = map[string]bool{}
	}
	// Each planner's orders are carried out as its own, within its reach.
	var callers []control.Caller
	byCaller := map[control.Caller]*decisions{}
	var results []control.Result
	for _, o := range msg.orders {
		if o.ID != "" && m.orch.orders[o.ID] {
			results = append(results, control.Result{Order: o.ID, Outcome: control.AlreadyApplied, Detail: "this order was carried out before"})
			continue
		}
		if o.ID != "" {
			m.orch.orders[o.ID] = true
		}
		if o.By.Group == "" {
			o.By = control.Caller{}
		}
		d := byCaller[o.By]
		if d == nil {
			d = &decisions{}
			byCaller[o.By] = d
			callers = append(callers, o.By)
		}
		switch o.Action {
		case control.Message:
			d.Reply = append(d.Reply, replyDecision{o.Task, o.Text, o.ID})
		case control.Approve:
			d.Approve = append(d.Approve, answerDecision{o.Item, o.Task, o.Text, "", o.ID})
		case control.Deny:
			d.Deny = append(d.Deny, answerDecision{o.Item, o.Task, o.Text, o.Note, o.ID})
		case control.Finish:
			d.Mark = append(d.Mark, markDecision{o.Task, "finished", o.Note, o.ID})
		case control.Fail:
			d.Mark = append(d.Mark, markDecision{o.Task, "failed", o.Note, o.ID})
		case control.Post:
			d.Post = append(d.Post, postDecision{o.Plan, o.Group, o.To, o.Text, o.ID})
		case control.Merge:
			d.Merge = append(d.Merge, planDecision{o.Plan, "", o.ID})
		case control.Close:
			d.Close = append(d.Close, planDecision{o.Plan, o.Note, o.ID})
		case control.Test:
			d.Test = append(d.Test, testDecision{o.Task, o.Text, o.ID})
		default:
			results = append(results, control.Result{Order: o.ID, Outcome: control.Failed, Detail: "grove doesn't know the action " + o.Action})
		}
	}
	var cmds []tea.Cmd
	for _, c := range callers {
		var by *session // the lead
		if c.Group != "" {
			by = m.crews[crewKey(c.Plan, c.Group)]
			if by == nil {
				// Gone since; it still acts only on its group.
				by = &session{planner: true, crewPlan: c.Plan, crewGroup: c.Group}
			}
		}
		d, refused := m.scopeDecisions(by, *byCaller[c])
		cmd, outs := m.decide(by, d)
		cmds = append(cmds, cmd)
		for _, o := range append(refused, outs...) {
			if o.order != "" {
				results = append(results, control.Result{Order: o.order, Outcome: o.result, Detail: o.detail})
			}
		}
	}
	cmd := tea.Batch(cmds...)
	dir, ok := m.controlDir()
	if !ok || len(results) == 0 {
		return cmd
	}
	now := time.Now()
	for i := range results {
		results[i].Time = now
	}
	return tea.Batch(cmd, func() tea.Msg {
		if err := control.Record(dir, results); err != nil {
			return actionMsg{err: fmt.Errorf("couldn't tell the planner what came of its orders: %w", err)}
		}
		return nil
	})
}
