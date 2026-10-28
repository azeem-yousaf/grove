package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/open"
	"github.com/charmbracelet/x/ansi"
)

// session is one worktree's agent: its conversation and what it changed.
// Each worktree has at most one, and an agent only ever works in its own
// worktree's folder.
type session struct {
	kind  agent.Kind
	path  string // worktree folder, the agent's working directory
	name  string // branch, for display
	model string // "" for the agent's own default
	menu  *modelMenu

	id      string // Claude session to resume
	entries []chatEntry
	history []agent.Turn // for agents without resume
	stream  string       // reply text still arriving

	running   bool
	started   time.Time
	cancel    context.CancelFunc
	events    chan agentMsg
	lastUser  string
	lastReply strings.Builder

	checkpoint string          // tree of the worktree before the agent's first turn
	touched    map[string]bool // repo-relative files the agent changed
	allow      []string        // permission rules the user approved
	pending    []pendingAction // approvals and suggested commands, oldest first
	approved   []string        // commands approved in the current batch
	cost       string
	failed     bool
	seen       bool // the user has looked since it last finished
}

type chatRole uint8

const (
	roleUser chatRole = iota
	roleAgent
	roleTool
	roleNote
	roleError
)

type chatEntry struct {
	role chatRole
	text string
}

type pendingKind uint8

const (
	pendingApproval pendingKind = iota // the agent was denied a command
	pendingCommand                     // the agent suggests the user runs one
)

type pendingAction struct {
	kind pendingKind
	text string
	tool string // the shell tool an approval is for: "Bash" or "PowerShell"
}

type (
	// agentMsg carries one agent event, or the checkpoint taken before the
	// first turn, back to the UI.
	agentMsg struct {
		path       string
		ev         agent.Event
		checkpoint string
		err        error
		closed     bool
	}
	agentSavedMsg struct{ err error }
	undoReadyMsg  struct {
		path    string
		changes []git.ChangeSince
		err     error
	}
	undoDoneMsg struct {
		path string
		n    int
		err  error
	}
	quitNowMsg struct{}
)

// picker asks which agent to use when several are installed, or explains
// that none is.
type picker struct {
	kinds    []agent.Kind
	cursor   int
	remember bool
	then     func(agent.Kind) tea.Cmd
}

// undoAsk confirms putting a worktree back the way it was before its agent.
type undoAsk struct {
	path    string
	name    string
	tree    string
	changes []git.ChangeSince
	running bool
}

// withAgent runs then with the agent to use, asking first if needed.
func (m *Model) withAgent(then func(agent.Kind) tea.Cmd) tea.Cmd {
	installed := m.installed()
	if k, ok := agent.Parse(m.cfg.AI.Agent); ok {
		for _, i := range installed {
			if i == k {
				return then(k)
			}
		}
	}
	if len(installed) == 1 {
		return then(installed[0])
	}
	m.picker = &picker{kinds: installed, remember: true, then: then}
	return nil
}

// chooseAgent always offers the choice (ctrl+a), unless there is none.
func (m *Model) chooseAgent(current agent.Kind, then func(agent.Kind) tea.Cmd) tea.Cmd {
	installed := m.installed()
	if len(installed) < 2 {
		return m.notify("Only "+current.Label()+" is installed", toastInfo)
	}
	p := &picker{kinds: installed, remember: true, then: then}
	for i, k := range installed {
		if k == current {
			p.cursor = i
		}
	}
	m.picker = p
	return nil
}

func (m *Model) onPickerKey(msg tea.KeyPressMsg) tea.Cmd {
	p := m.picker
	switch msg.String() {
	case "esc", "q":
		m.picker = nil
		return nil
	case "ctrl+c":
		return m.quitNow()
	}
	if len(p.kinds) == 0 {
		if msg.String() == "enter" {
			m.picker = nil
		}
		return nil
	}
	switch msg.String() {
	case "up", "k":
		p.cursor = max(p.cursor-1, 0)
	case "down", "j":
		p.cursor = min(p.cursor+1, len(p.kinds)-1)
	case "space", "r":
		p.remember = !p.remember
	case "1", "2":
		if i := int(msg.String()[0] - '1'); i < len(p.kinds) {
			p.cursor = i
			return m.pick()
		}
	case "enter":
		return m.pick()
	}
	return nil
}

func (m *Model) pick() tea.Cmd {
	p := m.picker
	k := p.kinds[p.cursor]
	m.picker = nil
	cmds := []tea.Cmd{p.then(k)}
	if p.remember {
		m.cfg.AI.Agent = string(k)
		save := m.saveAgent
		cmds = append(cmds, func() tea.Msg { return agentSavedMsg{save(string(k))} })
	}
	return tea.Batch(cmds...)
}

// sessionFor finds the session working in path.
func (m *Model) sessionFor(path string) *session {
	for p, s := range m.sessions {
		if git.SamePath(p, path) {
			return s
		}
	}
	return nil
}

func (m *Model) anyAgentRunning() bool {
	for _, s := range m.sessions {
		if s.running {
			return true
		}
	}
	return false
}

// openChat shows the selected worktree's agent, starting one if needed.
func (m *Model) openChat() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	st := m.snap.States[r.wt]
	if st.Bare || st.Prunable || st.Err != nil {
		return m.notify("That worktree has no folder for an agent to work in", toastInfo)
	}
	if s := m.sessionFor(st.Path); s != nil {
		return m.showChat(s)
	}
	path, name := st.Path, st.Name()
	return m.withAgent(func(k agent.Kind) tea.Cmd {
		return m.showChat(m.newSession(path, name, k))
	})
}

func (m *Model) newSession(path, name string, k agent.Kind) *session {
	s := &session{kind: k, path: path, name: name, model: m.cfg.AI.ModelFor(string(k)), touched: map[string]bool{}, seen: true}
	m.sessions[path] = s
	return s
}

func (m *Model) showChat(s *session) tea.Cmd {
	m.chatPath = s.path
	m.focus = focusChat
	s.seen = true
	m.chatFollow = true
	m.chatSel = chatSelection{}
	m.chatInput.SetValue("")
	return tea.Batch(m.chatInput.Focus(), m.warm(s))
}

// chatSession is the session shown in the chat pane.
func (m *Model) chatSession() *session {
	if m.chatPath == "" {
		return nil
	}
	return m.sessionFor(m.chatPath)
}

func (m *Model) onChatKey(msg tea.KeyPressMsg) tea.Cmd {
	s := m.chatSession()
	if s == nil {
		m.focus = focusList
		return nil
	}
	key := msg.String()
	if !m.chatSel.dragging {
		m.chatSel = chatSelection{}
	}
	switch key {
	case "ctrl+c":
		return m.quitNow()
	case "tab", "shift+tab":
		m.focus = focusList
		return nil
	case "pgup":
		m.chatView.ScrollUp(max(m.chatView.Height()/2, 1))
		m.chatFollow = false
		return nil
	case "pgdown":
		m.chatView.ScrollDown(max(m.chatView.Height()/2, 1))
		m.chatFollow = m.chatView.AtBottom()
		return nil
	case "ctrl+x":
		if s.running && s.cancel != nil {
			s.cancel()
		}
		return nil
	case "ctrl+z":
		return m.askUndo(s)
	case "ctrl+y":
		return m.copyLastReply(s)
	case "ctrl+a":
		if s.running {
			return m.notify("Stop the agent first (ctrl+x)", toastInfo)
		}
		path, name := s.path, s.name
		return m.chooseAgent(s.kind, func(k agent.Kind) tea.Cmd {
			if k == s.kind {
				return nil
			}
			ns := m.newSession(path, name, k)
			ns.checkpoint, ns.touched = s.checkpoint, s.touched
			ns.entries = []chatEntry{{roleNote, "Switched to " + k.Label() + ". It starts a new conversation."}}
			stop := m.stopAgent
			return tea.Batch(func() tea.Msg { stop(path); return nil }, m.showChat(ns))
		})
	}

	if s.menu != nil {
		return m.onModelMenuKey(s, key)
	}
	if len(s.pending) > 0 {
		return m.onPendingKey(s, key)
	}
	switch key {
	case "esc":
		m.focus = focusList
		return nil
	case "enter":
		text := strings.TrimSpace(m.chatInput.Value())
		if cmd, arg, _ := strings.Cut(text, " "); cmd == "/model" {
			m.chatInput.SetValue("")
			return m.modelCommand(s, strings.TrimSpace(arg))
		}
		if text == "" || s.running {
			return nil
		}
		m.chatInput.SetValue("")
		return m.send(s, text)
	}
	var cmd tea.Cmd
	m.chatInput, cmd = m.chatInput.Update(msg)
	return cmd
}

// copyLastReply puts the agent's latest reply on the clipboard as it wrote
// it, Markdown and all.
func (m *Model) copyLastReply(s *session) tea.Cmd {
	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].role == roleAgent {
			return m.copyText(s.entries[i].text, "Copied the agent's reply")
		}
	}
	return m.notify("The agent hasn't replied yet", toastInfo)
}

// copyText puts text on the clipboard, through the terminal (OSC 52) and
// the system clipboard both, since either can be unavailable.
func (m *Model) copyText(text, done string) tea.Cmd {
	return tea.Batch(
		tea.SetClipboard(text),
		action(func() (string, error) {
			_ = open.Copy(text)
			return done, nil
		}),
	)
}

// modelMenu offers models for a session's agent (/model).
type modelMenu struct {
	options []string // "" is the agent's own default
	cursor  int
}

// modelChoices are the models /model offers; any other name can be typed
// after it. Claude Code resolves these aliases to its latest models.
func modelChoices(k agent.Kind) []string {
	if k == agent.Claude {
		return []string{"", "fable", "opus", "sonnet", "haiku"}
	}
	return []string{""}
}

// modelName is how a model is shown: its name, or "default".
func modelName(model string) string {
	if model == "" {
		return "default"
	}
	return model
}

// modelCommand handles /model: on its own it opens the menu, with a name it
// switches to that model, and "default" goes back to the agent's own.
func (m *Model) modelCommand(s *session, arg string) tea.Cmd {
	if arg != "" {
		if strings.EqualFold(arg, "default") {
			arg = ""
		}
		return m.setModel(s, arg)
	}
	menu := &modelMenu{options: modelChoices(s.kind)}
	if i := slices.Index(menu.options, s.model); i >= 0 {
		menu.cursor = i
	} else {
		menu.options = append(menu.options, s.model)
		menu.cursor = len(menu.options) - 1
	}
	s.menu = menu
	return nil
}

func (m *Model) onModelMenuKey(s *session, key string) tea.Cmd {
	menu := s.menu
	switch key {
	case "left", "up", "h", "k":
		menu.cursor = max(menu.cursor-1, 0)
	case "right", "down", "l", "j":
		menu.cursor = min(menu.cursor+1, len(menu.options)-1)
	case "enter":
		return m.setModel(s, menu.options[menu.cursor])
	case "esc", "q":
		s.menu = nil
	}
	return nil
}

// setModel switches s to model from its next message, and remembers it for
// new chats with the same agent, the way /model does in Claude Code.
func (m *Model) setModel(s *session, model string) tea.Cmd {
	s.menu = nil
	s.model = model
	note := "Model: " + modelName(model)
	if model == "" && s.kind == agent.Claude {
		note += " (what /model is set to in Claude Code)"
	}
	if s.running {
		note += ". It applies from your next message"
	}
	s.entries = append(s.entries, chatEntry{roleNote, note + "."})
	m.chatFollow = true
	switch s.kind {
	case agent.Claude:
		m.cfg.AI.ClaudeModel = model
	case agent.Codex:
		m.cfg.AI.CodexModel = model
	}
	save, kind := m.saveModel, string(s.kind)
	return tea.Batch(func() tea.Msg { return agentSavedMsg{save(kind, model)} }, m.warm(s))
}

// onPendingKey handles the approval or suggested-command card.
func (m *Model) onPendingKey(s *session, key string) tea.Cmd {
	a := s.pending[0]
	next := func() tea.Cmd {
		s.pending = s.pending[1:]
		return m.afterPending(s)
	}
	switch key {
	case "esc", "n":
		if a.kind == pendingApproval {
			s.entries = append(s.entries, chatEntry{roleNote, "Denied: " + a.text})
		}
		return next()
	case "enter", "y":
		if a.kind == pendingApproval {
			s.allow = append(s.allow, agent.ClaudeRule(a.tool, a.text))
			s.approved = append(s.approved, a.text)
			s.entries = append(s.entries, chatEntry{roleNote, "Allowed: " + a.text})
			return next()
		}
		cmd := m.runSuggested(s, a.text)
		return tea.Batch(next(), cmd)
	case "e":
		if a.kind == pendingCommand {
			s.pending = s.pending[1:]
			m.focus = focusList
			return m.editSuggested(s, a.text)
		}
	}
	return nil
}

// afterPending continues the agent once every approval has an answer.
func (m *Model) afterPending(s *session) tea.Cmd {
	for _, p := range s.pending {
		if p.kind == pendingApproval {
			return nil
		}
	}
	if len(s.approved) == 0 {
		return nil
	}
	cmds := "`" + strings.Join(s.approved, "`, `") + "`"
	s.approved = nil
	return m.send(s, "I approved "+cmds+". Run it now and carry on.")
}

func (m *Model) runSuggested(s *session, command string) tea.Cmd {
	if rest, ok := strings.CutPrefix(command, "git "); ok {
		return m.runInTerminal(s.name, s.path, "git", splitCommand(rest), command)
	}
	prog, args := shellCommand(command)
	return m.runInTerminal(s.name, s.path, prog, args, command)
}

// editSuggested puts a suggested git command into the : prompt.
func (m *Model) editSuggested(s *session, command string) tea.Cmd {
	st := git.State{Worktree: git.Worktree{Path: s.path, Branch: s.name}}
	if i := m.snapIndex(s.path); i >= 0 {
		st = m.snap.States[i]
	}
	g := &m.gitCmd
	g.active, g.target = true, st
	g.hist, g.draft = len(g.history), ""
	g.input.SetValue(strings.TrimPrefix(command, "git "))
	g.input.CursorEnd()
	return g.input.Focus()
}

func (m *Model) snapIndex(path string) int {
	if m.snap == nil {
		return -1
	}
	return m.snap.IndexOf(path)
}

// send starts one turn of the agent.
func (m *Model) send(s *session, text string) tea.Cmd {
	s.entries = append(s.entries, chatEntry{roleUser, text})
	s.lastUser = text
	s.lastReply.Reset()
	s.stream = ""
	s.running, s.failed, s.started = true, false, time.Now()
	m.chatFollow = true

	ctx, cancel := context.WithCancel(m.ctx)
	s.cancel = cancel
	ch := make(chan agentMsg, 64)
	s.events = ch
	req := m.agentRequest(s)
	req.Message = text
	// Claude keeps the conversation itself, so it needs the instructions
	// once; Codex starts fresh every turn.
	if s.id == "" || s.kind == agent.Codex {
		req.Context = m.agentContext(s)
	}
	path, needCheckpoint := s.path, s.checkpoint == ""
	run := m.runAgent
	go func() {
		defer close(ch)
		if needCheckpoint {
			tree, err := git.Checkpoint(ctx, path)
			if err == nil {
				// Pinned so gc keeps it for undo after grove restarts. Undo
				// still works this session if pinning fails.
				_ = git.KeepCheckpoint(ctx, path, path, tree)
			}
			ch <- agentMsg{path: path, checkpoint: tree, err: err}
		}
		run(ctx, req, func(ev agent.Event) { ch <- agentMsg{path: path, ev: ev} })
	}()
	return tea.Batch(listen(ch), m.startAnimating())
}

// agentRequest describes s's next turn, without the message.
func (m *Model) agentRequest(s *session) agent.Request {
	return agent.Request{
		Kind:    s.kind,
		Dir:     s.path,
		Session: s.id,
		Allow:   append([]string(nil), s.allow...),
		Model:   s.model,
		History: append([]agent.Turn(nil), s.history...),
	}
}

// warm gets s's agent started while the user types, when it can be.
func (m *Model) warm(s *session) tea.Cmd {
	if s.running || len(s.pending) > 0 {
		return nil // the next turn's permissions aren't known yet
	}
	req, warm := m.agentRequest(s), m.warmAgent
	return func() tea.Msg {
		warm(req)
		return nil
	}
}

// listen delivers the next event from a running agent.
func listen(ch chan agentMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return agentMsg{closed: true}
		}
		return msg
	}
}

func (m *Model) onAgent(msg agentMsg) tea.Cmd {
	if msg.closed {
		return nil
	}
	s := m.sessionFor(msg.path)
	if s == nil {
		return nil
	}
	next := listen(s.events)
	if msg.checkpoint != "" || msg.err != nil {
		if msg.err != nil {
			s.entries = append(s.entries, chatEntry{roleNote, "Couldn't take a checkpoint, so undo won't be available: " + msg.err.Error()})
		}
		s.checkpoint = msg.checkpoint
		return next
	}
	ev := msg.ev
	switch ev.Kind {
	case agent.EvSession:
		s.id = ev.Session
	case agent.EvDelta:
		s.stream += ev.Text
	case agent.EvText:
		s.stream = ""
		s.entries = append(s.entries, chatEntry{roleAgent, ev.Text})
		if s.lastReply.Len() > 0 {
			s.lastReply.WriteString("\n\n")
		}
		s.lastReply.WriteString(ev.Text)
	case agent.EvTool:
		s.entries = append(s.entries, chatEntry{roleTool, toolLine(s, ev)})
		if ev.Path != "" {
			if rel, inside := relTo(s.path, ev.Path); inside {
				s.touched[rel] = true
			}
		}
	case agent.EvDone:
		return m.onAgentDone(s, ev)
	}
	return next
}

func (m *Model) onAgentDone(s *session, ev agent.Event) tea.Cmd {
	s.running, s.cancel, s.stream = false, nil, ""
	if ev.Session != "" {
		s.id = ev.Session
	}
	if ev.Cost != "" {
		s.cost = ev.Cost
	}
	if ev.Err != nil {
		s.failed = true
		s.entries = append(s.entries, chatEntry{roleError, ev.Err.Error()})
	}
	reply := s.lastReply.String()
	s.history = append(s.history, agent.Turn{User: s.lastUser, Agent: reply})

	for _, d := range ev.Denied {
		if d.Tool == "Bash" || d.Tool == "PowerShell" {
			if d.Detail != "" {
				s.pending = append(s.pending, pendingAction{kind: pendingApproval, text: d.Detail, tool: d.Tool})
			}
			continue
		}
		// Files outside this worktree stay off limits; there's nothing to
		// approve.
		what := d.Detail
		if rel, inside := relTo(s.path, d.Detail); inside {
			what = rel
		}
		s.entries = append(s.entries, chatEntry{roleNote, fmt.Sprintf("Blocked %s %s: outside this worktree", d.Tool, what)})
	}
	for _, c := range suggestedCommands(reply) {
		s.pending = append(s.pending, pendingAction{kind: pendingCommand, text: c})
	}

	viewing := m.focus == focusChat && m.chatSession() == s
	s.seen = viewing
	cmds := []tea.Cmd{m.startScan()}
	if !viewing {
		text, kind := "✦ "+s.name+": "+s.kind.Label()+" finished", toastOK
		switch {
		case ev.Err != nil:
			text, kind = "✦ "+s.name+": "+ev.Err.Error(), toastErr
		case len(s.pending) > 0:
			text, kind = "✦ "+s.name+" needs you", toastInfo
		}
		cmds = append(cmds, m.notify(text, kind))
	}
	return tea.Batch(cmds...)
}

// relTo makes p relative to dir with forward slashes, reporting whether it
// is inside dir.
func relTo(dir, p string) (string, bool) {
	if !isAbs(p) {
		p = filepath.Join(dir, filepath.FromSlash(p))
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p, false
	}
	return filepath.ToSlash(rel), true
}

// isAbs also treats a rooted path such as \src\x as absolute, which it is
// for every practical purpose even without a drive letter.
func isAbs(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)
}

func toolLine(s *session, ev agent.Event) string {
	detail := ev.Detail
	if isAbs(detail) {
		if rel, inside := relTo(s.path, detail); inside {
			detail = rel
		}
	}
	return strings.TrimSpace(ev.Tool + " " + strings.ReplaceAll(detail, "\n", " "))
}

var groveRun = regexp.MustCompile("(?s)```grove-run[ \\t]*\\r?\\n(.*?)```")

// suggestedCommands extracts the commands the agent asked the user to run.
func suggestedCommands(reply string) []string {
	var out []string
	for _, m := range groveRun.FindAllStringSubmatch(reply, -1) {
		for _, line := range strings.Split(m[1], "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
				out = append(out, line)
			}
		}
	}
	return out
}

// agentContext is the briefing sent with a session's first turn.
func (m *Model) agentContext(s *session) string {
	var b strings.Builder
	b.WriteString("# You are working inside grove\n\n")
	fmt.Fprintf(&b, "grove is a terminal app for git worktrees. You are the agent for one worktree: the folder %s, on branch %s.", s.path, s.name)
	if m.base != "" {
		fmt.Fprintf(&b, " Its changes are compared with %s.", m.base)
	}
	b.WriteString("\n\nRules:\n")
	b.WriteString("- Read and change files only inside your folder. ")
	var others []string
	if m.snap != nil {
		for _, st := range m.snap.States {
			if !git.SamePath(st.Path, s.path) {
				others = append(others, st.Path+" ("+st.Name()+")")
			}
		}
	}
	if len(others) > 0 {
		b.WriteString("These are other worktrees of the same repository, used for other work; never read or change anything in them: " + strings.Join(others, "; ") + ".")
	}
	b.WriteString("\n- Don't run git commands that change the repository (commit, checkout, switch, merge, rebase, reset, restore, stash, push, pull, branch). When one is needed, put it in a fenced code block whose language is grove-run, one command per line, and the user will run it from grove:\n\n```grove-run\ngit commit -m \"Describe the change\"\n```\n")
	b.WriteString("- Some shell commands need the user's approval. If one is refused, say what you wanted to run and why, then stop.\n")
	b.WriteString("- Replies appear in a narrow terminal panel: keep them short and plain.\n")

	if i := m.snapIndex(s.path); i >= 0 {
		st := m.snap.States[i]
		if len(st.Files) > 0 {
			fmt.Fprintf(&b, "\n## Changes in this worktree so far (%s, +%d -%d)\n\n", plural(len(st.Files), "file"), st.Added, st.Deleted)
			for i, f := range st.Files {
				if i == 40 {
					fmt.Fprintf(&b, "... and %d more\n", len(st.Files)-40)
					break
				}
				fmt.Fprintf(&b, "%c %s +%d -%d\n", f.Status, f.Path, f.Added, f.Deleted)
			}
		}
	}
	return b.String()
}

// askUndo lists what changed since the agent's checkpoint and asks to put
// it back.
func (m *Model) askUndo(s *session) tea.Cmd {
	if s == nil || s.checkpoint == "" {
		return m.notify("Nothing to undo: no agent has worked in this worktree yet", toastInfo)
	}
	if s.running {
		return m.notify("Stop the agent first (ctrl+x)", toastInfo)
	}
	path, tree := s.path, s.checkpoint
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
		defer cancel()
		changes, err := git.ChangedSince(ctx, path, tree)
		return undoReadyMsg{path, changes, err}
	}
}

func (m *Model) onUndoReady(msg undoReadyMsg) tea.Cmd {
	s := m.sessionFor(msg.path)
	if s == nil {
		return nil
	}
	if msg.err != nil {
		return m.notify("Couldn't compare with the checkpoint: "+msg.err.Error(), toastErr)
	}
	if len(msg.changes) == 0 {
		return m.notify("Nothing changed since "+s.kind.Label()+" started in "+s.name, toastInfo)
	}
	m.undo = &undoAsk{path: s.path, name: s.name, tree: s.checkpoint, changes: msg.changes}
	return nil
}

func (m *Model) onUndoKey(msg tea.KeyPressMsg) tea.Cmd {
	u := m.undo
	if u.running {
		return nil
	}
	switch msg.String() {
	case "esc", "n", "q":
		m.undo = nil
	case "ctrl+c":
		return m.quitNow()
	case "enter", "y":
		u.running = true
		path, tree, changes := u.path, u.tree, u.changes
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
			defer cancel()
			err := git.RestoreCheckpoint(ctx, path, tree, changes)
			return undoDoneMsg{path, len(changes), err}
		}
	}
	return nil
}

func (m *Model) onUndoDone(msg undoDoneMsg) tea.Cmd {
	m.undo = nil
	if s := m.sessionFor(msg.path); s != nil {
		s.touched = map[string]bool{}
		note := fmt.Sprintf("Undid %s since the agent started.", plural(msg.n, "changed file"))
		if msg.err != nil {
			note = "Undo finished with errors: " + msg.err.Error()
		}
		s.entries = append(s.entries, chatEntry{roleNote, note})
	}
	if msg.err != nil {
		return tea.Batch(m.notify("Undo: "+msg.err.Error(), toastErr), m.startScan())
	}
	return tea.Batch(m.notify("Put back "+plural(msg.n, "file")+" as they were before the agent", toastOK), m.startScan())
}

// quit leaves grove, asking first while agents are working.
func (m *Model) quit() tea.Cmd {
	if m.anyAgentRunning() {
		m.quitAsk = true
		return nil
	}
	m.persistNow()
	return tea.Quit
}

// quitNow stops every agent and quits, giving the agent processes a moment
// to be killed before grove exits.
func (m *Model) quitNow() tea.Cmd {
	m.persistNow() // a working agent is saved as interrupted
	running := false
	for _, s := range m.sessions {
		if s.running && s.cancel != nil {
			s.cancel()
			running = true
		}
	}
	if !running {
		return tea.Quit
	}
	return tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return quitNowMsg{} })
}

func (m *Model) onQuitKey(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "enter", "y", "q", "ctrl+c":
		return m.quitNow()
	case "esc", "n":
		m.quitAsk = false
	}
	return nil
}

// agentIcon is the worktree's status dot while it has an agent: coloured by
// what the agent is doing.
func (m *Model) agentIcon(path string) string {
	s := m.sessionFor(path)
	if s == nil {
		return ""
	}
	switch {
	case s.running:
		return m.st.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)])
	case len(s.pending) > 0:
		return m.st.warn.Bold(true).Render("✦")
	case s.failed:
		return m.st.del.Render("✦")
	case !s.seen:
		return m.st.add.Render("✦")
	}
	return m.st.accent.Render("✦")
}

// agentBadge is the worktree header's agent status in words.
func (m *Model) agentBadge(path string) string {
	s := m.sessionFor(path)
	if s == nil {
		return ""
	}
	st := m.st
	switch {
	case s.running:
		return st.accent.Render("✦ " + spinnerFrames[m.frame%len(spinnerFrames)] + " working " + since(m.now, s.started))
	case len(s.pending) > 0:
		return st.warn.Bold(true).Render("✦ needs you")
	case s.failed:
		return st.del.Render("✦ failed")
	case !s.seen:
		return st.add.Render("✦ done")
	}
	return st.faint.Render("✦ " + string(s.kind))
}

func since(now, t time.Time) string {
	d := now.Sub(t)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// chatPane draws the agent conversation in place of the diff.
func (m *Model) chatPane(w, h int) string {
	s := m.chatSession()
	st := m.st
	inner := w - 2
	title := "✦ " + s.name + " · " + s.kind.Label()
	if s.model != "" {
		title += " · " + s.model
	}
	info := ""
	switch {
	case s.running:
		info = st.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)] + " working " + since(m.now, s.started))
	case s.cost != "":
		info = st.faint.Render(s.cost)
	}

	bottom := m.chatBottom(s, inner)
	m.chatView.SetWidth(inner)
	m.chatView.SetHeight(max(h-2-len(bottom), 1))
	m.chatView.SetContentLines(m.highlightChat(m.chatLines(s, inner), inner))
	if m.chatFollow {
		m.chatView.GotoBottom()
	}
	body := append(strings.Split(m.chatView.View(), "\n"), bottom...)
	return m.panel(title, info, "", body, w, h, m.focus == focusChat)
}

// chatBottom is the card waiting for the user, or the input line.
func (m *Model) chatBottom(s *session, width int) []string {
	st := m.st
	rule := st.faint.Render(strings.Repeat("─", width))
	if menu := s.menu; menu != nil {
		dflt := "the agent's own setting"
		if s.kind == agent.Claude {
			dflt = "your /model in Claude Code"
		}
		head := st.accent.Bold(true).Render(" Model for "+s.kind.Label()) + st.faint.Render("  default = "+dflt)
		var opts []string
		for i, o := range menu.options {
			if i == menu.cursor {
				opts = append(opts, st.accent.Bold(true).Render("▸ "+modelName(o)))
			} else {
				opts = append(opts, st.text.Render("  "+modelName(o)))
			}
		}
		return []string{
			rule,
			head,
			" " + strings.Join(opts, " "),
			" " + m.hints([][2]string{{"←→", "choose"}, {"enter", "use"}, {"esc", "cancel"}, {"/model <name>", "any other"}}),
		}
	}
	if len(s.pending) > 0 {
		a := s.pending[0]
		head, keys := st.warn.Bold(true).Render(" "+s.kind.Label()+" wants to run"), [][2]string{{"enter", "allow"}, {"esc", "deny"}}
		if a.kind == pendingCommand {
			head, keys = st.accent.Bold(true).Render(" Suggested command"), [][2]string{{"enter", "run"}, {"e", "edit"}, {"esc", "skip"}}
		}
		more := ""
		if n := len(s.pending) - 1; n > 0 {
			more = st.faint.Render(fmt.Sprintf("  +%d more", n))
		}
		return []string{
			rule,
			head + more,
			" " + st.text.Bold(true).Render(ansi.Truncate(a.text, width-2, "…")),
			" " + m.hints(keys),
		}
	}
	m.chatInput.SetWidth(max(width-4, 10))
	hint := [][2]string{{"enter", "send"}, {"shift+enter", "new line"}, {"ctrl+y", "copy reply"}, {"tab", "list"}, {"ctrl+z", "undo all"}, {"/model", "model"}}
	if s.running {
		hint = [][2]string{{"ctrl+x", "stop"}, {"ctrl+y", "copy reply"}, {"tab", "list"}, {"/model", "model"}}
	}
	out := []string{rule}
	for i, l := range strings.Split(m.chatInput.View(), "\n") {
		mark := "  "
		if i == 0 {
			mark = st.accent.Render("▸ ")
		}
		out = append(out, " "+mark+l)
	}
	return append(out, " "+ansi.Truncate(m.hints(hint), width-1, ""))
}

// chatLines renders the conversation for the viewport.
func (m *Model) chatLines(s *session, width int) []string {
	st := m.st
	var out []string
	if len(s.entries) == 0 && !s.running {
		out = append(out, "",
			"  "+st.bold.Render("Ask "+s.kind.Label()+" to work on "+s.name),
			"",
			"  "+st.muted.Render("It can read and edit files in this worktree only. Shell"),
			"  "+st.muted.Render("commands and git changes wait for your approval, and"),
			"  "+st.muted.Render("ctrl+z puts everything back the way it was."),
			"",
			"  "+st.muted.Render("Model: "+modelName(s.model)+". Type /model to change it."))
		return out
	}
	add := func(prefix lipgloss.Style, label string, body string, style lipgloss.Style) {
		lines := renderChatText(body, max(width-7, 10), st, style) // a column clear of the border
		for i, l := range lines {
			p := "      "
			if i == 0 {
				p = " " + prefix.Render(fmt.Sprintf("%-5s", label))
			}
			out = append(out, p+l)
		}
	}
	for _, e := range s.entries {
		switch e.role {
		case roleUser:
			out = append(out, "")
			add(st.accent.Bold(true), "you", e.text, st.text)
		case roleAgent:
			out = append(out, "")
			add(st.add.Bold(true), "✦", e.text, st.text)
		case roleTool:
			out = append(out, "      "+st.faint.Render(ansi.Truncate("↳ "+e.text, width-7, "…")))
		case roleNote:
			add(st.faint, "", e.text, st.muted)
		case roleError:
			add(st.del.Bold(true), "✕", e.text, st.del)
		}
	}
	if s.stream != "" {
		out = append(out, "")
		add(st.add.Bold(true), "✦", s.stream, st.text)
	}
	if s.running {
		out = append(out, "", "      "+st.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)])+st.faint.Render(" "+s.kind.Label()+" is working…"))
	}
	return out
}

// renderChatText renders a message's Markdown to width: prose is wrapped
// with its inline formatting applied, tables are laid out in columns, and
// fenced code blocks are kept as they are.
func renderChatText(body string, width int, st styles, style lipgloss.Style) []string {
	var out []string
	inCode, lang := false, ""
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			if inCode {
				lang = strings.TrimPrefix(trimmed, "```")
			}
			continue
		}
		if inCode {
			code := st.info
			if lang == "grove-run" {
				code = st.warn.Bold(true)
			}
			out = append(out, code.Render(ansi.Truncate("  "+expandTabs(l), width, "…")))
			continue
		}
		if n := tableLen(lines[i:]); n > 0 {
			out = append(out, renderTable(lines[i:i+n], width, st, style)...)
			i += n - 1
			continue
		}
		if trimmed == "" {
			out = append(out, "")
			continue
		}
		out = append(out, renderProse(l, width, st, style)...)
	}
	return out
}

// pickerModal asks which agent to use, or explains how to get one.
func (m *Model) pickerModal() string {
	st := m.st
	p := m.picker
	var parts []string
	if len(p.kinds) == 0 {
		parts = []string{
			st.bold.Foreground(st.p.accent).Render("No AI assistant found"),
			"",
			st.muted.Render("grove uses an AI tool already on your PC. Install one,"),
			st.muted.Render("sign in once, and come back:"),
			"",
			"  " + st.text.Render("Claude Code  ") + st.faint.Render("https://claude.com/claude-code"),
			"  " + st.text.Render("Codex        ") + st.faint.Render("https://github.com/openai/codex"),
			"",
			m.hints([][2]string{{"esc", "close"}}),
		}
	} else {
		parts = []string{
			st.bold.Foreground(st.p.accent).Render("Choose an AI assistant"),
			"",
			st.muted.Render("grove found more than one AI tool on this PC."),
			st.muted.Render("Which should the worktree agents use?"),
			"",
		}
		for i, k := range p.kinds {
			line := fmt.Sprintf("  %d  %-13s", i+1, k.Label())
			s := st.text
			mark := "  "
			if i == p.cursor {
				s, mark = st.accent.Bold(true), st.accent.Render("▸ ")
			}
			parts = append(parts, mark+s.Render(line)+st.faint.Render(string(k)))
		}
		box := st.faint.Render("[ ]")
		if p.remember {
			box = st.accent.Bold(true).Render("[x]")
		}
		parts = append(parts, "", box+" "+st.text.Render("Remember my choice")+st.faint.Render("  (change it later with ctrl+a)"), "",
			m.hints([][2]string{{"enter", "choose"}, {"1/2", "pick"}, {"space", "remember"}, {"esc", "cancel"}}))
	}
	return st.modal.Render(strings.Join(parts, "\n"))
}

// undoModal confirms undoing an agent's work.
func (m *Model) undoModal() string {
	st := m.st
	u := m.undo
	parts := []string{
		st.bold.Foreground(st.p.del).Render("Undo the agent's changes"),
		"",
		st.muted.Render("Put " + u.name + " back the way it was before the agent"),
		st.muted.Render("started. These files changed since then:"),
		"",
	}
	room := max(m.height-16, 3)
	for i, c := range u.changes {
		if i == room && len(u.changes) > room+1 {
			parts = append(parts, st.faint.Render(fmt.Sprintf("  … and %d more", len(u.changes)-room)))
			break
		}
		what := map[git.Status]string{git.Added: "delete", git.Deleted: "bring back", git.Modified: "restore"}[c.Status]
		parts = append(parts, "  "+statusStyle(st, c.Status).Bold(true).Render(string(c.Status))+" "+st.text.Render(ansi.Truncate(c.Path, 50, "…"))+st.faint.Render("  "+what))
	}
	parts = append(parts, "", st.faint.Render("Edits you made yourself since then are undone too."), "")
	if u.running {
		parts = append(parts, st.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)])+st.muted.Render(" Restoring…"))
	} else {
		parts = append(parts, m.hints([][2]string{{"enter", "undo"}, {"esc", "keep the changes"}}))
	}
	return st.modal.BorderForeground(st.p.del).Render(strings.Join(parts, "\n"))
}

func (m *Model) quitModal() string {
	st := m.st
	n := 0
	for _, s := range m.sessions {
		if s.running {
			n++
		}
	}
	text := fmt.Sprintf("%d agents are working. Quitting stops them.", n)
	if n == 1 {
		text = "An agent is working. Quitting stops it."
	}
	return st.modal.Render(strings.Join([]string{
		st.bold.Foreground(st.p.warn).Render("Agents are still working"),
		"",
		st.muted.Render(text),
		"",
		m.hints([][2]string{{"enter", "stop and quit"}, {"esc", "stay"}}),
	}, "\n"))
}

// newChatInput is the message box. It grows with what is typed or pasted,
// up to chatInputMax lines, and keeps pasted line breaks. Enter sends, so
// a new line is shift+enter, or ctrl+j where the terminal can't tell
// shift+enter apart.
func newChatInput() textarea.Model {
	ta := textarea.New()
	ta.Prompt = ""
	ta.Placeholder = "ask the agent to do something in this worktree…"
	ta.ShowLineNumbers = false
	ta.DynamicHeight = true
	ta.MinHeight = 1
	ta.MaxHeight = chatInputMax
	ta.MaxContentHeight = 5000 // visual rows; without it MaxHeight caps the lines
	ta.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter", "ctrl+j", "alt+enter"))
	return ta
}

const chatInputMax = 6

// chatInputStyles drops the textarea's cursor-line highlight, which looks
// like a selection in a one-line box.
func chatInputStyles(dark bool) textarea.Styles {
	s := textarea.DefaultStyles(dark)
	s.Focused.CursorLine = lipgloss.NewStyle()
	s.Blurred.CursorLine = lipgloss.NewStyle()
	s.Focused.EndOfBuffer = lipgloss.NewStyle()
	s.Blurred.EndOfBuffer = lipgloss.NewStyle()
	return s
}

func newChatView() viewport.Model { return viewport.New() }

// defaults for the agent hooks, replaced in tests.
func defaultSaveAgent(name string) error { return config.SaveAgent(name) }
