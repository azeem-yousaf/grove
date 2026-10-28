package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/git"
)

// reopen starts a second grove on the same repository and loads its saved
// sessions, as if grove had been closed and opened again.
func reopen(t *testing.T, m *Model) *Model {
	t.Helper()
	repo := *m.repo
	m2 := New(context.Background(), Options{Repo: &repo, Config: config.Default(), Version: "test"})
	noAgents(m2)
	m2.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	if cmd := m2.loadSessions(); cmd != nil {
		m2.Update(cmd())
	}
	return m2
}

func TestSessionsSurviveRestart(t *testing.T) {
	m, mainDir := mergeModel(t)
	fake := &fakeAgent{turns: [][]agent.Event{{
		{Kind: agent.EvText, Text: "Done.\n\n```grove-run\ngit commit -am x\n```"},
		{Kind: agent.EvDone, Session: "sess-1", Cost: "$0.01"},
	}}}
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	m.runAgent = fake.run
	press(m, "a")
	typeText(m, "fix the tests")
	pump(t, m, func() tea.Msg { _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); return cmd() })
	s := m.chatSession()
	if s.checkpoint == "" || s.id != "sess-1" || len(s.pending) != 1 {
		t.Fatalf("session after a turn: checkpoint %q, id %q, pending %v", s.checkpoint, s.id, s.pending)
	}
	s.allow = []string{"Bash(npm test)"}
	s.touched["shared.txt"] = true

	// The clock saves in the background.
	cmd := m.persistSessions()
	if cmd == nil {
		t.Fatal("a changed session should be saved")
	}
	msg := cmd().(persistMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	m.Update(msg)
	if m.persistSessions() != nil {
		t.Error("an unchanged session shouldn't be written again")
	}
	ref, _ := exec.Command("git", "-C", mainDir, "for-each-ref", "--format=%(objectname)", "refs/grove/checkpoints").Output()
	if strings.TrimSpace(string(ref)) != s.checkpoint {
		t.Errorf("checkpoint ref = %q, want %s", ref, s.checkpoint)
	}

	m2 := reopen(t, m)
	r := m2.sessionFor(mainDir)
	if r == nil {
		t.Fatal("the session wasn't restored")
	}
	if r.id != "sess-1" || r.kind != agent.Claude || r.checkpoint != s.checkpoint || r.cost != "$0.01" ||
		len(r.allow) != 1 || !r.touched["shared.txt"] || len(r.history) != 1 || r.running {
		t.Errorf("restored %+v", r)
	}
	if len(r.pending) != 1 || r.pending[0].kind != pendingCommand || r.pending[0].text != "git commit -am x" {
		t.Errorf("restored pending = %+v", r.pending)
	}
	if len(r.entries) != len(s.entries) || r.entries[0].role != roleUser || r.entries[0].text != "fix the tests" {
		t.Errorf("restored entries = %+v", r.entries)
	}

	// Claude picks the conversation up by its session ID, without being
	// briefed again.
	fake2 := &fakeAgent{}
	m2.installed = m.installed
	m2.runAgent = fake2.run
	m2.showChat(r)
	r.pending = nil
	typeText(m2, "and the docs")
	pump(t, m2, func() tea.Msg { _, cmd := m2.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); return cmd() })
	if len(fake2.reqs) != 1 || fake2.reqs[0].Session != "sess-1" || fake2.reqs[0].Context != "" || len(fake2.reqs[0].Allow) != 1 {
		t.Errorf("resumed request = %+v", fake2.reqs)
	}
}

func TestInterruptedAndStaleSessions(t *testing.T) {
	m, mainDir := mergeModel(t)
	s := m.newSession(mainDir, "main", agent.Claude)
	s.entries = []chatEntry{{roleUser, "go"}}
	s.running = true
	s.checkpoint = strings.Repeat("1", 40) // a tree gc has removed
	gone := filepath.Join(t.TempDir(), "gone")
	m.newSession(gone, "feature/gone", agent.Codex)
	m.persistNow()
	if _, err := os.Stat(filepath.Join(m.store.Dir(), sessionFile(gone))); err != nil {
		t.Fatalf("session not saved: %v", err)
	}

	m2 := reopen(t, m)
	r := m2.sessionFor(mainDir)
	if r == nil || r.running || r.checkpoint != "" {
		t.Fatalf("restored %+v", r)
	}
	notes := ""
	for _, e := range r.entries {
		notes += e.text + "\n"
	}
	if !strings.Contains(notes, "cut short") || !strings.Contains(notes, "undo isn't available") {
		t.Errorf("notes = %q", notes)
	}
	// A session whose folder is gone is dropped from disk too.
	if m2.sessionFor(gone) != nil {
		t.Error("a session for a missing folder was restored")
	}
	if _, err := os.Stat(filepath.Join(m.store.Dir(), sessionFile(gone))); !os.IsNotExist(err) {
		t.Errorf("stale session file left: %v", err)
	}
}

func TestRemovingWorktreeForgetsSession(t *testing.T) {
	m, _ := mergeModel(t)
	i := -1
	for j, st := range m.snap.States {
		if st.Branch == "feature/c" {
			i = j
		}
	}
	dir := m.snap.States[i].Path
	ctx := context.Background()
	tree, err := git.Checkpoint(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := git.KeepCheckpoint(ctx, dir, dir, tree); err != nil {
		t.Fatal(err)
	}
	s := m.newSession(dir, "feature/c", agent.Claude)
	s.checkpoint = tree
	m.persistNow()

	res, err := git.RemoveWorktree(ctx, m.repo, m.snap.States[i].Worktree, git.RemoveOptions{})
	_, cmd := m.Update(removeMsg{r: &removal{st: m.snap.States[i]}, res: res, err: err})
	for _, msg := range runCmds(cmd) {
		if p, ok := msg.(persistMsg); ok && p.err != nil {
			t.Fatal(p.err)
		}
	}
	if m.sessionFor(dir) != nil {
		t.Error("the removed worktree's session is still there")
	}
	if _, err := os.Stat(filepath.Join(m.store.Dir(), sessionFile(dir))); !os.IsNotExist(err) {
		t.Errorf("session file left: %v", err)
	}
	if out, _ := exec.Command("git", "-C", m.repo.CommonDir, "for-each-ref", "refs/grove").Output(); len(out) != 0 {
		t.Errorf("checkpoint ref left: %s", out)
	}
}
