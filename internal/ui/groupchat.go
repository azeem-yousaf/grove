package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/charmbracelet/x/ansi"
)

// Grouped tasks' agents share a group chat (package group), reached through
// grove's own MCP tool server, which only they are given. grove shows the
// chat, lets the user post in it, and wakes idle agents that have messages
// waiting, at most cfg.Planner.Wakeups() times in a row per group before
// the user posts again, so agents can't keep each other going forever.

// groupState is what grove last read of one group.
type groupState struct {
	msgs    []group.Message
	unread  map[string]int // by member, from their read cursors
	wakeups int            // automatic wake-ups since the user last posted
	paused  bool           // wake-ups stopped at the limit
}

// groupChat is the group chat pane, shown in place of the diff.
type groupChat struct {
	plan, group string
	input       textarea.Model
	view        viewport.Model
	follow      bool
}

type groupsReadMsg struct {
	states map[string]groupRead // by group folder
}

type groupRead struct {
	msgs   []group.Message
	unread map[string]int
}

// groupLog is the chat of a plan's group, or false without a store.
func (m *Model) groupLog(p *plan.Plan, groupID string) (group.Log, bool) {
	if m.store == nil || groupID == "" {
		return group.Log{}, false
	}
	return group.Log{Dir: group.Dir(m.store.Dir(), p.ID, groupID)}, true
}

// groupTools gives a grouped task's agent its group chat. Agents of
// isolated tasks get nothing.
func (m *Model) groupTools(s *session) []agent.ToolServer {
	p, t := m.taskFor(s.path)
	if t == nil || t.Group == "" || m.groveBin == "" {
		return nil
	}
	log, ok := m.groupLog(p, t.Group)
	if !ok {
		return nil
	}
	return []agent.ToolServer{{
		Name:    "grove",
		Command: m.groveBin,
		Args:    []string{"mcp-group", "--dir", log.Dir, "--as", t.Branch},
		Allow:   group.Tools,
	}}
}

// groupmateDirs are the worktrees of s's groupmates that exist, which its
// agent may read but not change.
func (m *Model) groupmateDirs(s *session) []string {
	p, t := m.taskFor(s.path)
	if t == nil || t.Group == "" {
		return nil
	}
	var out []string
	for _, o := range p.Members(t.Group) {
		if o.ID != t.ID && o.Path != "" {
			out = append(out, o.Path)
		}
	}
	return out
}

// groupInfo is who is in a group, for the members tool.
func groupInfo(p *plan.Plan, g plan.Group) group.Info {
	info := group.Info{Plan: p.Title, Name: g.Name}
	for _, t := range p.Members(g.ID) {
		info.Members = append(info.Members, group.Member{Name: t.Branch, Task: t.Title, Brief: t.Brief, Path: t.Path, Status: string(t.Status)})
	}
	return info
}

// writeGroupInfo refreshes every group's member list of a started plan.
func (m *Model) writeGroupInfo(p *plan.Plan) tea.Cmd {
	if m.store == nil || p.Status == plan.Draft || len(p.Groups) == 0 {
		return nil
	}
	type job struct {
		log  group.Log
		info group.Info
	}
	var jobs []job
	for _, g := range p.Groups {
		if log, ok := m.groupLog(p, g.ID); ok {
			jobs = append(jobs, job{log, groupInfo(p, g)})
		}
	}
	return func() tea.Msg {
		var errs []error
		for _, j := range jobs {
			if err := j.log.WriteInfo(j.info); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			return persistMsg{err: errs[0]}
		}
		return nil
	}
}

// groupContext tells a grouped agent about its group and its tools.
func (m *Model) groupContext(p *plan.Plan, t *plan.Task) string {
	g, ok := p.Group(t.Group)
	if !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Your group: %s\n\nYou are %s in a group chat with the agents of these tasks:\n", g.Name, t.Branch)
	for _, o := range p.Members(g.ID) {
		if o.ID == t.ID {
			continue
		}
		where := "not started yet"
		if o.Path != "" {
			where = "worktree " + o.Path
		}
		fmt.Fprintf(&b, "- %s: %s (%s)\n", o.Branch, o.Title, where)
	}
	b.WriteString("\nUse grove's tools for it: `members` lists the group, `post` sends a message (to everyone, or one member with `to`), " +
		"`read` gets messages sent to you (`wait_seconds` waits for an answer). Read your messages when you start, before you say you are done, " +
		"and whenever you need something from a groupmate. Post when you settle anything they depend on: APIs, types, names, file layout, decisions. " +
		"Keep messages short and specific. The user can read and post in the chat too; their name is \"you\". " +
		"grove itself posts as \"grove\", for example when your worktree starts conflicting with another.\n")
	if watched(p) {
		b.WriteString("The planner is watching this plan for the user and reads the chat; it posts as \"planner\". " +
			"Post to everyone when you need a decision from it.\n")
	}
	return b.String()
}

// pollGroups reads every group of the started plans, for the chat pane and
// for waking agents with messages waiting.
func (m *Model) pollGroups() tea.Cmd {
	type member struct {
		log  group.Log
		name string
	}
	var logs []group.Log
	var members []member
	for _, p := range m.plans {
		if p.Status == plan.Draft {
			continue
		}
		for _, g := range p.Groups {
			log, ok := m.groupLog(p, g.ID)
			if !ok {
				continue
			}
			logs = append(logs, log)
			for _, t := range p.Members(g.ID) {
				members = append(members, member{log, t.Branch})
			}
		}
	}
	if len(logs) == 0 || m.pollingGroups {
		return nil
	}
	m.pollingGroups = true
	return func() tea.Msg {
		out := map[string]groupRead{}
		for _, l := range logs {
			msgs, _ := l.All()
			out[l.Dir] = groupRead{msgs: msgs, unread: map[string]int{}}
		}
		for _, mb := range members {
			if n, err := mb.log.Unread(mb.name); err == nil {
				out[mb.log.Dir].unread[mb.name] = len(n)
			}
		}
		return groupsReadMsg{out}
	}
}

func (m *Model) onGroupsRead(msg groupsReadMsg) tea.Cmd {
	m.pollingGroups = false
	for dir, r := range msg.states {
		g := m.groups[dir]
		if g == nil {
			g = &groupState{}
			m.groups[dir] = g
		}
		g.msgs, g.unread = r.msgs, r.unread
	}
	m.queueGroups()
	return tea.Batch(m.wakeGroups(), m.orchestrate())
}

// wakeGroups starts a turn for each idle grouped agent with messages
// waiting, until its group reaches the wake-up limit.
func (m *Model) wakeGroups() tea.Cmd {
	var cmds []tea.Cmd
	for _, p := range m.plans {
		if p.Status == plan.Draft {
			continue
		}
		for _, grp := range p.Groups {
			log, ok := m.groupLog(p, grp.ID)
			g := m.groups[log.Dir]
			if !ok || g == nil {
				continue
			}
			for _, t := range p.Members(grp.ID) {
				s := m.sessionFor(t.Path)
				if t.Path == "" || s == nil || s.running || len(s.pending) > 0 || g.unread[t.Branch] == 0 {
					continue
				}
				if g.wakeups >= m.cfg.Planner.Wakeups() {
					if !g.paused {
						g.paused = true
						cmds = append(cmds, m.notify(fmt.Sprintf("%s: paused waking agents after %d messages in a row; post in the group chat (t) to go on", groupTitle(grp), g.wakeups), toastInfo))
					}
					continue
				}
				g.wakeups++
				n := g.unread[t.Branch]
				g.unread[t.Branch] = 0 // until the next poll sees what it read
				cmds = append(cmds, m.send(s, fmt.Sprintf("You have %s in your group chat. Read them with grove's read tool, answer if they need it, and carry on with your task.", plural(n, "new message"))))
			}
		}
	}
	return tea.Batch(cmds...)
}

// openGroupChat shows the group chat of the task in the worktree at path.
func (m *Model) openGroupChat(path string) tea.Cmd {
	p, t := m.taskFor(path)
	if t == nil || t.Group == "" {
		return m.notify("That worktree isn't part of a group", toastInfo)
	}
	return m.openGroup(p.ID, t.Group)
}

// openGroup shows a plan group's chat.
func (m *Model) openGroup(planID, groupID string) tea.Cmd {
	if m.store == nil {
		return nil
	}
	ta := newChatInput()
	ta.Placeholder = "message the group; @branch to address one agent…"
	ta.SetStyles(chatInputStyles(m.dark))
	m.groupChat = &groupChat{plan: planID, group: groupID, input: ta, view: viewport.New(), follow: true}
	m.focus = focusGroup
	return tea.Batch(m.groupChat.input.Focus(), m.pollGroups())
}

func (m *Model) onGroupKey(msg tea.KeyPressMsg) tea.Cmd {
	gc := m.groupChat
	if gc == nil {
		m.focus = focusList
		return nil
	}
	switch msg.String() {
	case "ctrl+c":
		return m.quitNow()
	case "tab", "shift+tab", "esc":
		m.focus = focusList
		return nil
	case "pgup":
		gc.view.ScrollUp(max(gc.view.Height()/2, 1))
		gc.follow = false
		return nil
	case "pgdown":
		gc.view.ScrollDown(max(gc.view.Height()/2, 1))
		gc.follow = gc.view.AtBottom()
		return nil
	case "enter":
		text := strings.TrimSpace(gc.input.Value())
		if text == "" {
			return nil
		}
		gc.input.SetValue("")
		return m.postToGroup(text)
	}
	var cmd tea.Cmd
	gc.input, cmd = gc.input.Update(msg)
	return cmd
}

// postToGroup posts the user's message. "@branch text" goes to one member.
// Posting resets the group's wake-up count.
func (m *Model) postToGroup(text string) tea.Cmd {
	gc := m.groupChat
	p := m.planByID(gc.plan)
	if p == nil {
		return nil
	}
	log, ok := m.groupLog(p, gc.group)
	if !ok {
		return nil
	}
	m.userActed()
	msg := group.Message{From: group.User, Text: text}
	if name, rest, found := strings.Cut(text, " "); found && strings.HasPrefix(name, "@") {
		for _, t := range p.Members(gc.group) {
			if t.Branch == name[1:] {
				msg.To, msg.Text = t.Branch, rest
			}
		}
	}
	if g := m.groups[log.Dir]; g != nil {
		g.wakeups, g.paused = 0, false
	}
	gc.follow = true
	return func() tea.Msg {
		if err := log.Post(msg); err != nil {
			return actionMsg{err: fmt.Errorf("couldn't post: %w", err)}
		}
		return groupPostedMsg{}
	}
}

type groupPostedMsg struct{}

// groupPane draws the group chat.
func (m *Model) groupPane(w, h int) string {
	gc, st := m.groupChat, m.st
	inner := w - 2
	p := m.planByID(gc.plan)
	if p == nil {
		return m.panel("Group chat", "", "", nil, w, h, true)
	}
	g, _ := p.Group(gc.group)
	log, _ := m.groupLog(p, gc.group)
	state := m.groups[log.Dir]

	var lines []string
	var who []string
	for _, t := range p.Members(gc.group) {
		badge := st.faint.Render(string(t.Status))
		if s := m.sessionFor(t.Path); s != nil && s.running {
			badge = st.accent.Render("working")
		}
		who = append(who, st.text.Render(t.Branch)+" "+badge)
	}
	lines = append(lines, " "+st.muted.Render("Members: ")+strings.Join(who, st.faint.Render(" · ")))
	if state == nil || len(state.msgs) == 0 {
		lines = append(lines, "", "  "+st.muted.Render("No messages yet. The agents post here when they need each"),
			"  "+st.muted.Render("other; you can post too, or @branch to address one agent."))
	} else {
		for _, msg := range state.msgs {
			name := st.bold.Render(msg.From)
			switch msg.From {
			case group.User:
				name = st.accent.Bold(true).Render("you")
			case group.Grove:
				name = st.warn.Bold(true).Render("grove")
			}
			head := " " + st.faint.Render(msg.Time.Local().Format("15:04")) + " " + name
			if msg.To != "" {
				head += st.faint.Render(" → " + msg.To)
			}
			lines = append(lines, "", head)
			for _, l := range renderChatText(msg.Text, max(inner-4, 10), st, st.text) {
				lines = append(lines, "   "+l)
			}
		}
	}
	if state != nil && state.paused {
		lines = append(lines, "", " "+st.warn.Render(ansi.Truncate(fmt.Sprintf("Paused: agents were woken %d times in a row. Post to let them go on.", state.wakeups), inner-2, "…")))
	}

	bottom := []string{st.faint.Render(strings.Repeat("─", inner))}
	gc.input.SetWidth(max(inner-4, 10))
	for i, l := range strings.Split(gc.input.View(), "\n") {
		mark := "  "
		if i == 0 {
			mark = st.accent.Render("▸ ")
		}
		bottom = append(bottom, " "+mark+l)
	}
	bottom = append(bottom, " "+ansi.Truncate(m.hints([][2]string{{"enter", "post"}, {"@branch", "to one"}, {"tab", "list"}, {"pgup/pgdn", "scroll"}}), inner-1, ""))
	gc.view.SetWidth(inner)
	gc.view.SetHeight(max(h-2-len(bottom), 1))
	gc.view.SetContentLines(lines)
	if gc.follow {
		gc.view.GotoBottom()
	}
	body := append(strings.Split(gc.view.View(), "\n"), bottom...)
	return m.panel(groupTitle(*g)+" · "+p.Title, st.faint.Render(plural(len(p.Members(gc.group)), "agent")), "", body, w, h, m.focus == focusGroup)
}

// groveExecutable is the program agents start for the group tools.
func groveExecutable() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return p
}

// pollGroupsEvery keeps the group chats fresh while the clock ticks.
func (m *Model) pollGroupsEvery(now time.Time) tea.Cmd {
	if now.Sub(m.lastGroupPoll) < time.Second {
		return nil
	}
	m.lastGroupPoll = now
	return m.pollGroups()
}
