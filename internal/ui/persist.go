package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/store"
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
	Touched     []string       `json:"touched,omitempty"`
	Allow       []string       `json:"allow,omitempty"`
	Pending     []savedPending `json:"pending,omitempty"`
	Cost        string         `json:"cost,omitempty"`
	Failed      bool           `json:"failed,omitempty"`
	Interrupted bool           `json:"interrupted,omitempty"` // saved while the agent was working
}

type savedEntry struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

type savedPending struct {
	Command bool   `json:"command,omitempty"` // a suggested command rather than an approval
	Text    string `json:"text"`
	Tool    string `json:"tool,omitempty"`
}

var roleNames = map[chatRole]string{roleUser: "user", roleAgent: "agent", roleTool: "tool", roleNote: "note", roleError: "error"}

func sessionFile(path string) string { return "sessions/" + git.PathKey(path) + ".json" }

func (s *session) saved() savedSession {
	out := savedSession{
		Version: 1, Kind: s.kind, Path: s.path, Name: s.name, Model: s.model, ID: s.id,
		History: s.history, Checkpoint: s.checkpoint, Allow: s.allow, Cost: s.cost,
		Failed: s.failed, Interrupted: s.running,
	}
	for _, e := range s.entries {
		out.Entries = append(out.Entries, savedEntry{roleNames[e.role], e.text})
	}
	for p := range s.touched {
		out.Touched = append(out.Touched, p)
	}
	slices.Sort(out.Touched)
	for _, p := range s.pending {
		out.Pending = append(out.Pending, savedPending{Command: p.kind == pendingCommand, Text: p.text, Tool: p.tool})
	}
	return out
}

// restore turns a saved session back into a live one, explaining anything
// that couldn't come back.
func restore(v savedSession, checkpointOK bool) *session {
	s := &session{
		kind: v.Kind, path: v.Path, name: v.Name, model: v.Model, id: v.ID,
		history: v.History, checkpoint: v.Checkpoint, allow: v.Allow, cost: v.Cost,
		failed: v.Failed, touched: map[string]bool{}, seen: true,
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
		s.pending = append(s.pending, pendingAction{kind: kind, text: p.Text, tool: p.Tool})
	}
	if v.Interrupted {
		s.entries = append(s.entries, chatEntry{roleNote, "grove was closed while the agent was working, so that turn was cut short."})
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
	persistMsg struct{ err error }
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
			if _, err := os.Stat(v.Path); errors.Is(err, os.ErrNotExist) {
				_ = st.Remove(n)
				_ = git.DropCheckpoint(ctx, common, v.Path)
				continue
			}
			ok := v.Checkpoint == "" || git.HasObject(ctx, common, v.Checkpoint)
			out = append(out, restore(v, ok))
		}
		return sessionsLoadedMsg{out, errors.Join(errs...)}
	}
}

func (m *Model) onSessionsLoaded(msg sessionsLoadedMsg) tea.Cmd {
	for _, s := range msg.sessions {
		// One the user started while loading wins.
		if m.sessionFor(s.path) != nil {
			continue
		}
		m.sessions[s.path] = s
		if b, err := json.Marshal(s.saved()); err == nil {
			m.savedJSON[s.path] = b
		}
	}
	if msg.err != nil {
		return m.notify("Couldn't restore some agent conversations: "+msg.err.Error(), toastErr)
	}
	return nil
}

// changedSessions marshals the sessions that differ from what was last
// written, and records them as written.
func (m *Model) changedSessions() map[string][]byte {
	if m.store == nil {
		return nil
	}
	out := map[string][]byte{}
	for path, s := range m.sessions {
		b, err := json.Marshal(s.saved())
		if err != nil || bytes.Equal(b, m.savedJSON[path]) {
			continue
		}
		m.savedJSON[path] = b
		out[path] = b
	}
	return out
}

func writeSessions(st *store.Store, changed map[string][]byte) error {
	var errs []error
	for path, b := range changed {
		errs = append(errs, st.Write(sessionFile(path), b))
	}
	return errors.Join(errs...)
}

// persistSessions saves changed sessions in the background. The clock calls
// it every second, so a crash loses at most a second of conversation.
func (m *Model) persistSessions() tea.Cmd {
	changed := m.changedSessions()
	if len(changed) == 0 {
		return nil
	}
	st := m.store
	return func() tea.Msg { return persistMsg{writeSessions(st, changed)} }
}

// persistNow saves changed sessions before grove exits.
func (m *Model) persistNow() {
	if changed := m.changedSessions(); len(changed) > 0 {
		_ = writeSessions(m.store, changed)
	}
}

func (m *Model) onPersisted(msg persistMsg) tea.Cmd {
	if msg.err == nil {
		m.persistErr = ""
		return nil
	}
	if msg.err.Error() == m.persistErr {
		return nil // already said so
	}
	m.persistErr = msg.err.Error()
	// Try again next time rather than trusting what wasn't written.
	m.savedJSON = map[string][]byte{}
	return m.notify("Couldn't save agent conversations: "+msg.err.Error(), toastErr)
}

// forgetSession deletes a worktree's agent, its saved conversation and its
// checkpoint, once the worktree itself is gone.
func (m *Model) forgetSession(path string) tea.Cmd {
	for p := range m.sessions {
		if git.SamePath(p, path) {
			delete(m.sessions, p)
			delete(m.savedJSON, p)
		}
	}
	if m.chatPath != "" && git.SamePath(m.chatPath, path) {
		m.chatPath = ""
		if m.focus == focusChat {
			m.focus = focusList
		}
	}
	st, common := m.store, m.repo.CommonDir
	if st == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		_ = git.DropCheckpoint(ctx, common, path)
		return persistMsg{st.Remove(sessionFile(path))}
	}
}
