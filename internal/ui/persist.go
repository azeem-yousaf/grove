package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/store"
	"github.com/azeem-yousaf/grove/internal/usage"
)

// savedSession is a worktree agent's session as stored on disk, so the
// conversation, its undo checkpoint and the commands the user allowed come
// back when grove is reopened. Claude resumes its own session by ID; Codex
// gets the history replayed, as it does every turn anyway.
type savedSession struct {
	Version     int            `json:"version"`
	Kind        agent.Kind     `json:"kind"`
	Path        string         `json:"path"`
	Name        string         `json:"name"`
	Model       string         `json:"model,omitempty"`
	ID          string         `json:"session,omitempty"`
	Entries     []savedEntry   `json:"entries"`
	History     []agent.Turn   `json:"history,omitempty"`
	Checkpoint  string         `json:"checkpoint,omitempty"`
	TurnTree    string         `json:"turn_tree,omitempty"`
	Touched     []string       `json:"touched,omitempty"`
	Allow       []string       `json:"allow,omitempty"`
	Pending     []savedPending `json:"pending,omitempty"`
	Approvals   int            `json:"approvals,omitempty"` // approvals so far, for their IDs
	Answered    []answerRec    `json:"answered,omitempty"`
	Notes       []string       `json:"notes,omitempty"` // news from grove not yet given to the agent
	Cost        string         `json:"cost,omitempty"`
	Failed      bool           `json:"failed,omitempty"`
	Interrupted bool           `json:"interrupted,omitempty"` // saved while the agent was working
	Planner     bool           `json:"planner,omitempty"`

	// Token usage (version 2). Version 1 sessions get a new UID on load.
	UID      string       `json:"uid,omitempty"`
	Created  time.Time    `json:"created,omitzero"`
	Turns    int          `json:"turns,omitempty"`
	Drafting []string     `json:"drafting,omitempty"` // the planner's turns with no plan yet
	Running  *runningTurn `json:"running_turn,omitempty"`
	// Claude's figures for the session ID above, so the first turn after a
	// restart is still exact.
	SessionTotal *usage.Tokens `json:"session_total,omitempty"`
	// Ledger writes that hadn't succeeded yet.
	Unrecorded []usage.Turn       `json:"unrecorded,omitempty"`
	Unassigned []usage.Assignment `json:"unassigned,omitempty"`
}

// runningTurn is the turn a session was saved in the middle of, recorded as
// cut short when it is loaded.
type runningTurn struct {
	ID     string        `json:"id"`
	Attr   turnAttr      `json:"attr"`
	Tokens *usage.Tokens `json:"tokens,omitempty"` // nil until the agent reported any
}

type savedEntry struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type savedPending struct {
	Command bool   `json:"command,omitempty"` // a suggested command rather than an approval
	ID      string `json:"id,omitempty"`
	Text    string `json:"text"`
	Tool    string `json:"tool,omitempty"`

	// What grove's policy said about an approval.
	UserOnly    bool     `json:"user_only,omitempty"`
	Reason      string   `json:"reason,omitempty"`
	Protected   bool     `json:"protected,omitempty"`
	Destructive bool     `json:"destructive,omitempty"`
	Refs        []string `json:"refs,omitempty"`
	Left        bool     `json:"left,omitempty"` // the planner left it for the user
}

var roleNames = map[chatRole]string{roleUser: "user", roleAgent: "agent", roleTool: "tool", roleNote: "note", roleError: "error"}

func sessionFile(path string) string { return "sessions/" + git.PathKey(path) + ".json" }

// plannerFile holds the planner's conversation.
const plannerFile = "planner/session.json"

// file is where s is saved.
func (s *session) file() string {
	if s.planner {
		return plannerFile
	}
	return sessionFile(s.path)
}

func (s *session) saved() savedSession {
	out := savedSession{
		Version: 2, Kind: s.kind, Path: s.path, Name: s.name, Model: s.model, ID: s.id,
		History: s.history, Checkpoint: s.checkpoint, TurnTree: s.turnTree, Allow: s.allow, Notes: s.notes, Cost: s.cost,
		Failed: s.failed, Interrupted: s.running, Planner: s.planner, Approvals: s.seq, Answered: s.answered,
		UID: s.uid, Created: s.created, Turns: s.turns, Drafting: s.drafting,
		Unrecorded: s.unrecorded, Unassigned: s.unassigned,
	}
	if !s.sessionTotal.IsZero() {
		t := s.sessionTotal
		out.SessionTotal = &t
	}
	if s.running && s.turnID != "" {
		out.Running = &runningTurn{ID: s.turnID, Attr: s.turnAttr}
		if s.liveSeen {
			t := s.live
			out.Running.Tokens = &t
		}
	}
	for _, e := range s.entries {
		out.Entries = append(out.Entries, savedEntry{roleNames[e.role], e.text})
	}
	for p := range s.touched {
		out.Touched = append(out.Touched, p)
	}
	slices.Sort(out.Touched)
	for _, p := range s.pending {
		pol := p.policy
		out.Pending = append(out.Pending, savedPending{Command: p.kind == pendingCommand, ID: p.id, Text: p.text, Tool: p.tool,
			UserOnly: pol.userOnly, Reason: pol.reason, Protected: pol.protected, Destructive: pol.destructive, Refs: pol.refs, Left: p.left})
	}
	return out
}

// restore turns a saved session back into a live one, explaining anything
// that couldn't come back.
func restore(v savedSession, checkpointOK, turnOK bool) *session {
	s := &session{
		kind: v.Kind, path: v.Path, name: v.Name, model: v.Model, id: v.ID,
		history: v.History, checkpoint: v.Checkpoint, allow: v.Allow, notes: v.Notes, cost: v.Cost,
		failed: v.Failed, touched: map[string]bool{}, seen: true, planner: v.Planner, seq: v.Approvals, answered: v.Answered,
		uid: v.UID, created: v.Created, turns: v.Turns, drafting: v.Drafting,
		unrecorded: v.Unrecorded, unassigned: v.Unassigned,
	}
	if s.uid == "" {
		s.uid = newUID() // saved before grove counted tokens
	}
	if v.SessionTotal != nil {
		s.sessionTotal = *v.SessionTotal
	}
	if v.Interrupted {
		s.cutShort = v.Running
	}
	for _, e := range v.Entries {
		role := roleNote
		for r, name := range roleNames {
			if name == e.Role {
				role = r
			}
		}
		s.entries = append(s.entries, chatEntry{role, e.Text})
	}
	for _, p := range v.Touched {
		s.touched[p] = true
	}
	for _, p := range v.Pending {
		kind := pendingApproval
		if p.Command {
			kind = pendingCommand
		}
		a := pendingAction{kind: kind, id: p.ID, text: p.Text, tool: p.Tool, left: p.Left,
			policy: approvalPolicy{userOnly: p.UserOnly, reason: p.Reason, protected: p.Protected, destructive: p.Destructive, refs: p.Refs}}
		if kind == pendingApproval && a.id == "" {
			// Saved by an older grove.
			a.id = s.nextApprovalID()
		}
		s.pending = append(s.pending, a)
	}
	if v.Interrupted {
		s.entries = append(s.entries, chatEntry{roleNote, "grove was closed while the agent was working, so that turn was cut short."})
	}
	if turnOK {
		s.turnTree = v.TurnTree // unpinned, so gc may have taken it
	}
	if s.checkpoint != "" && !checkpointOK {
		s.checkpoint = ""
		s.entries = append(s.entries, chatEntry{roleNote, "The checkpoint from before the agent started is gone, so undo isn't available."})
	}
	return s
}

type (
	sessionsLoadedMsg struct {
		sessions []*session
		err      error
	}
	persistMsg struct {
		written map[string][]byte // file name to what was written
		err     error
	}
)

// loadSessions reads the saved sessions. Sessions for worktrees whose folder
// is gone are deleted along with their checkpoint.
func (m *Model) loadSessions() tea.Cmd {
	st, common := m.store, m.repo.CommonDir
	if st == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		defer cancel()
		names, err := st.List("sessions")
		if _, perr := os.Stat(filepath.Join(st.Dir(), filepath.FromSlash(plannerFile))); perr == nil {
			names = append(names, plannerFile)
		}
		var out []*session
		var errs []error
		if err != nil {
			errs = append(errs, err)
		}
		for _, n := range names {
			var v savedSession
			if err := st.Load(n, &v); err != nil {
				errs = append(errs, err)
				continue
			}
			if _, err := os.Stat(v.Path); errors.Is(err, os.ErrNotExist) && !v.Planner {
				_ = st.Remove(n)
				_ = git.DropCheckpoint(ctx, common, v.Path)
				continue
			}
			ok := v.Checkpoint == "" || git.HasObject(ctx, common, v.Checkpoint)
			turnOK := v.TurnTree != "" && git.HasObject(ctx, common, v.TurnTree)
			out = append(out, restore(v, ok, turnOK))
		}
		return sessionsLoadedMsg{out, errors.Join(errs...)}
	}
}

func (m *Model) onSessionsLoaded(msg sessionsLoadedMsg) tea.Cmd {
	for _, s := range msg.sessions {
		// A turn cut short is saved again without it, so it is recorded once.
		cut := m.recordCutShort(s)
		// One the user started while loading wins.
		switch {
		case s.planner && m.planner == nil:
			m.planner = s
		case s.planner, m.sessionFor(s.path) != nil:
			continue
		default:
			m.sessions[s.path] = s
		}
		if b, err := json.Marshal(s.saved()); err == nil && !cut {
			m.savedJSON[s.file()] = b
		}
	}
	if msg.err != nil {
		return m.notify("Couldn't restore some agent conversations: "+msg.err.Error(), toastErr)
	}
	return nil
}

// changedSessions marshals the sessions and plans that differ from what
// was last written. Keys are store file names. They count as written only
// once the write succeeds, so one queued when grove quits isn't lost.
func (m *Model) changedSessions() map[string][]byte {
	if m.store == nil {
		return nil
	}
	out := map[string][]byte{}
	add := func(f string, v any) {
		if b, err := json.Marshal(v); err == nil && !bytes.Equal(b, m.savedJSON[f]) {
			out[f] = b
		}
	}
	for _, s := range m.allSessions() {
		add(s.file(), s.saved())
	}
	for _, p := range m.plans {
		add(planFile(p.ID), p)
	}
	return out
}

// writeState writes changed files, returning the ones written.
func writeState(st *store.Store, changed map[string][]byte) (map[string][]byte, error) {
	written := map[string][]byte{}
	var errs []error
	for name, b := range changed {
		if err := st.Write(name, b); err != nil {
			errs = append(errs, err)
			continue
		}
		written[name] = b
	}
	return written, errors.Join(errs...)
}

// persistSessions saves changed sessions and plans in the background. The
// clock calls it every second, so a crash loses at most a second of
// conversation.
func (m *Model) persistSessions() tea.Cmd {
	changed := m.changedSessions()
	if len(changed) == 0 {
		return nil
	}
	st := m.store
	return func() tea.Msg {
		written, err := writeState(st, changed)
		return persistMsg{written: written, err: err}
	}
}

// persistNow saves what changed before grove exits.
func (m *Model) persistNow() {
	if changed := m.changedSessions(); len(changed) > 0 {
		written, err := writeState(m.store, changed)
		m.onPersisted(persistMsg{written: written, err: err})
	}
}

func (m *Model) onPersisted(msg persistMsg) tea.Cmd {
	for f, b := range msg.written {
		m.savedJSON[f] = b
	}
	if msg.err == nil {
		m.persistErr = ""
		return nil
	}
	if msg.err.Error() == m.persistErr {
		return nil // already said so
	}
	m.persistErr = msg.err.Error()
	return m.notify("Couldn't save grove's state: "+msg.err.Error(), toastErr)
}

// forgetSession deletes a worktree's agent, its saved conversation and its
// checkpoint, once the worktree itself is gone.
func (m *Model) forgetSession(path string) tea.Cmd {
	var recorded []tea.Cmd
	for p, s := range m.sessions {
		if git.SamePath(p, path) {
			// Its usage stays in the ledger; a turn still running is
			// recorded now, since its end will never be seen.
			recorded = append(recorded, m.abandonTurn(s))
			delete(m.sessions, p)
			delete(m.savedJSON, sessionFile(p))
		}
	}
	if !m.chatPlanner && m.chatPath != "" && git.SamePath(m.chatPath, path) {
		m.chatPath = ""
		if m.focus == focusChat {
			m.focus = focusList
		}
	}
	st, common := m.store, m.repo.CommonDir
	if st == nil {
		return tea.Batch(recorded...)
	}
	return tea.Batch(append(recorded, func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		_ = git.DropCheckpoint(ctx, common, path)
		return persistMsg{err: st.Remove(sessionFile(path))}
	})...)
}
