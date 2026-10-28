package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/git"
)

// fakeAgent records each request and replies with a script per turn.
type fakeAgent struct {
	mu    sync.Mutex
	reqs  []agent.Request
	turns [][]agent.Event
}

func (f *fakeAgent) run(_ context.Context, req agent.Request, emit func(agent.Event)) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	var evs []agent.Event
	if len(f.turns) > 0 {
		evs, f.turns = f.turns[0], f.turns[1:]
	}
	f.mu.Unlock()
	for _, ev := range evs {
		emit(ev)
	}
	if len(evs) == 0 || evs[len(evs)-1].Kind != agent.EvDone {
		emit(agent.Event{Kind: agent.EvDone})
	}
}

// pump feeds the agent's messages back into the model until it goes
// quiet. Other commands (toasts, ticks, terminal handovers) are dropped.
func pump(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	// Every command runs concurrently; ticks and toasts simply never matter.
	// The loop ends once nothing relevant has arrived for a while.
	results := make(chan tea.Msg, 256)
	start := func(c tea.Cmd) {
		if c != nil {
			go func() { results <- c() }()
		}
	}
	start(cmd)
	idle := time.NewTimer(3 * time.Second)
	deadline := time.After(30 * time.Second)
	for {
		select {
		case msg := <-results:
			switch msg := msg.(type) {
			case tea.BatchMsg:
				for _, c := range msg {
					start(c)
				}
			case agentMsg, undoReadyMsg, undoDoneMsg, agentSavedMsg:
				_, next := m.Update(msg)
				start(next)
				idle.Reset(3 * time.Second)
			}
		case <-idle.C:
			return
		case <-deadline:
			t.Error("pump: still busy after 30s")
			return
		}
	}
}

func TestAgentPicker(t *testing.T) {
	m := fakeModel(t, 140, 36)
	var saved []string
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude, agent.Codex} }
	m.saveAgent = func(s string) error { saved = append(saved, s); return nil }

	press(m, "a")
	if m.picker == nil {
		t.Fatal("with both installed and nothing chosen, a should ask")
	}
	if out := plain(m); !strings.Contains(out, "Choose an AI assistant") || !strings.Contains(out, "Remember my choice") {
		t.Errorf("picker not shown:\n%s", out)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: '2', Text: "2"})
	pump(t, m, cmd)
	s := m.chatSession()
	if m.picker != nil || s == nil || s.kind != agent.Codex || m.focus != focusChat {
		t.Fatalf("after choosing: picker=%v session=%+v focus=%v", m.picker, s, m.focus)
	}
	if m.cfg.AI.Agent != "codex" || len(saved) != 1 || saved[0] != "codex" {
		t.Errorf("choice not remembered: cfg=%q saved=%v", m.cfg.AI.Agent, saved)
	}

	// Another worktree uses the remembered agent without asking.
	press(m, "tab", "]", "a")
	if m.picker != nil || m.chatSession().kind != agent.Codex || m.chatSession() == s {
		t.Error("the remembered agent should be used for other worktrees")
	}

	// Nothing installed: explain.
	m2 := fakeModel(t, 140, 36)
	press(m2, "a")
	if m2.picker == nil || !strings.Contains(plain(m2), "No AI assistant found") {
		t.Error("with nothing installed, a should explain")
	}
	press(m2, "esc")
	if m2.picker != nil {
		t.Error("esc should close it")
	}

	// Exactly one: no question.
	m3 := fakeModel(t, 140, 36)
	m3.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	press(m3, "a")
	if m3.picker != nil || m3.chatSession() == nil || m3.chatSession().kind != agent.Claude {
		t.Error("with one agent installed it should be used straight away")
	}
}

func TestAgentConversation(t *testing.T) {
	m := fakeModel(t, 140, 36)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	auth := filepath.FromSlash("/src/shop-auth")
	fa := &fakeAgent{turns: [][]agent.Event{
		{
			{Kind: agent.EvSession, Session: "s1"},
			{Kind: agent.EvDelta, Text: "Work"},
			{Kind: agent.EvText, Text: "Working on it."},
			{Kind: agent.EvTool, Tool: "Edit", Detail: filepath.Join(auth, "src", "auth", "login.ts"), Path: filepath.Join(auth, "src", "auth", "login.ts")},
			{Kind: agent.EvText, Text: "Done. Commit it:\n\n```grove-run\ngit commit -m \"Add login\"\n```"},
			{Kind: agent.EvDone, Session: "s1", Cost: "$0.01", Denied: []agent.Denial{
				{Tool: "Bash", Detail: "npm test"},
				{Tool: "Read", Detail: filepath.FromSlash("/src/shop-cart/src/cart/cart.ts")},
			}},
		},
		{
			{Kind: agent.EvText, Text: "Tests pass."},
			{Kind: agent.EvDone, Session: "s1"},
		},
	}}
	m.runAgent = fa.run

	press(m, "a") // feature/auth is selected at start
	s := m.chatSession()
	if s == nil || s.name != "feature/auth" {
		t.Fatalf("session = %+v", s)
	}
	s.checkpoint = "tree" // skip git in this test
	typeText(m, "add a login page")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)

	if len(fa.reqs) != 1 {
		t.Fatalf("requests = %d", len(fa.reqs))
	}
	req := fa.reqs[0]
	if req.Message != "add a login page" || req.Dir != auth || req.Kind != agent.Claude || req.Session != "" {
		t.Errorf("first request = %+v", req)
	}
	for _, want := range []string{"never read or change anything in them", filepath.FromSlash("/src/shop-cart"), "grove-run", "feature/auth"} {
		if !strings.Contains(req.Context, want) {
			t.Errorf("briefing is missing %q:\n%s", want, req.Context)
		}
	}
	if s.id != "s1" || !s.touched["src/auth/login.ts"] || s.cost != "$0.01" {
		t.Errorf("session after turn: id=%q touched=%v cost=%q", s.id, s.touched, s.cost)
	}
	if len(s.pending) != 2 || s.pending[0].kind != pendingApproval || s.pending[0].text != "npm test" ||
		s.pending[1].kind != pendingCommand || s.pending[1].text != `git commit -m "Add login"` {
		t.Fatalf("pending = %+v", s.pending)
	}

	out := plain(m)
	for _, want := range []string{"Working on it.", "↳ Edit src/auth/login.ts", "Blocked Read", "outside this worktree", "Claude Code wants to run", "npm test", "enter allow"} {
		if !strings.Contains(out, want) {
			t.Errorf("chat is missing %q:\n%s", want, out)
		}
	}

	// The list shows the agent's status and the files it changed.
	press(m, "tab")
	out = plain(m)
	if !strings.Contains(out, "✦ needs you") || !strings.Contains(out, "✦ ● ") {
		t.Errorf("list is missing the agent badge or ✦ marker:\n%s", out)
	}

	// Allowing the command continues the same session with it permitted.
	press(m, "tab")
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)
	if len(fa.reqs) != 2 {
		t.Fatalf("allowing should continue the agent; requests = %d", len(fa.reqs))
	}
	req = fa.reqs[1]
	if req.Session != "s1" || req.Context != "" || !strings.Contains(req.Message, "npm test") {
		t.Errorf("follow-up = %+v", req)
	}
	found := false
	for _, r := range req.Allow {
		found = found || r == agent.ClaudeRule("npm test")
	}
	if !found {
		t.Errorf("follow-up should allow npm test: %v", req.Allow)
	}

	// The suggested commit is still waiting; esc skips it.
	if len(s.pending) != 1 || s.pending[0].kind != pendingCommand {
		t.Fatalf("pending after allow = %+v", s.pending)
	}
	if !strings.Contains(plain(m), "Suggested command") {
		t.Error("suggested command card missing")
	}
	press(m, "esc")
	if len(s.pending) != 0 || m.focus != focusChat {
		t.Errorf("esc should skip the card and stay in the chat: pending=%v focus=%v", s.pending, m.focus)
	}
	if !strings.Contains(plain(m), "Tests pass.") {
		t.Error("second reply missing")
	}
}

func TestAgentCodexReplaysHistory(t *testing.T) {
	m := fakeModel(t, 140, 36)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Codex} }
	fa := &fakeAgent{turns: [][]agent.Event{
		{{Kind: agent.EvText, Text: "First answer"}},
		{{Kind: agent.EvText, Text: "Second answer"}},
	}}
	m.runAgent = fa.run
	press(m, "a")
	m.chatSession().checkpoint = "tree"
	for _, q := range []string{"one", "two"} {
		typeText(m, q)
		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		pump(t, m, cmd)
	}
	if len(fa.reqs) != 2 {
		t.Fatalf("requests = %d", len(fa.reqs))
	}
	second := fa.reqs[1]
	if second.Context == "" || len(second.History) != 1 || second.History[0].User != "one" || second.History[0].Agent != "First answer" {
		t.Errorf("codex should get the briefing and history every turn: %+v", second)
	}
}

func TestAgentQuitAndStop(t *testing.T) {
	m := fakeModel(t, 140, 36)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	block := make(chan struct{})
	stopped := make(chan struct{})
	m.runAgent = func(ctx context.Context, _ agent.Request, emit func(agent.Event)) {
		select {
		case <-ctx.Done():
			close(stopped)
		case <-block:
		}
		emit(agent.Event{Kind: agent.EvDone, Err: ctx.Err()})
	}
	press(m, "a")
	m.chatSession().checkpoint = "tree"
	typeText(m, "go")
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.anyAgentRunning() {
		t.Fatal("agent should be running")
	}
	if !strings.Contains(plain(m), "working") {
		t.Error("chat should show the agent working")
	}

	press(m, "tab", "q")
	if !m.quitAsk || !strings.Contains(plain(m), "Agents are still working") {
		t.Fatal("q with a running agent should ask first")
	}
	press(m, "esc")
	if m.quitAsk {
		t.Fatal("esc should keep grove open")
	}

	// ctrl+x in the chat stops it.
	press(m, "tab")
	m.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("ctrl+x didn't stop the agent")
	}
	close(block)
}

// TestAgentUndo lets a fake agent edit a real worktree, then undoes it.
func TestAgentUndo(t *testing.T) {
	if git.Available() != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "shop")
	gitc := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(dir, 0o755)
	gitc("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(dir, "app.txt"), []byte("original\n"), 0o644)
	gitc("add", ".")
	gitc("commit", "-qm", "init")

	ctx := context.Background()
	repo, err := git.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	m := New(ctx, Options{Repo: repo, Config: config.Default(), Version: "test"})
	noAgents(m)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	m.runAgent = func(_ context.Context, req agent.Request, emit func(agent.Event)) {
		os.WriteFile(filepath.Join(req.Dir, "app.txt"), []byte("agent edit\n"), 0o644)
		os.WriteFile(filepath.Join(req.Dir, "new.txt"), []byte("new\n"), 0o644)
		emit(agent.Event{Kind: agent.EvTool, Tool: "Edit", Path: filepath.Join(req.Dir, "app.txt")})
		emit(agent.Event{Kind: agent.EvDone})
	}
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 36})
	m.Update(baseMsg{base: "main"})
	snap, err := git.Scan(ctx, repo, "main", git.ModeBranch)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(scanMsg{seq: m.scanSeq, snap: snap})

	press(m, "a")
	typeText(m, "change things")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)
	s := m.chatSession()
	if s.checkpoint == "" {
		t.Fatal("a checkpoint should be taken before the first turn")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "app.txt")); string(b) != "agent edit\n" {
		t.Fatal("fake agent didn't edit")
	}

	_, cmd = m.Update(tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl})
	pump(t, m, cmd)
	if m.undo == nil || len(m.undo.changes) != 2 {
		t.Fatalf("undo dialog = %+v", m.undo)
	}
	if out := plain(m); !strings.Contains(out, "Undo the agent's changes") || !strings.Contains(out, "new.txt") {
		t.Errorf("undo dialog not shown:\n%s", out)
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)
	if m.undo != nil {
		t.Error("dialog should close after undoing")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "app.txt")); string(b) != "original\n" {
		t.Errorf("app.txt = %q, want the original", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Error("new.txt should be gone")
	}
}

func TestCreateStartsAgentWithTask(t *testing.T) {
	m := fakeModel(t, 140, 36)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	fa := &fakeAgent{turns: [][]agent.Event{{{Kind: agent.EvText, Text: "On it"}}}}
	m.runAgent = fa.run
	press(m, "n")
	c := m.creating
	if !strings.Contains(plain(m), "Task") {
		t.Error("the dialog should offer a task when an agent is installed")
	}
	c.inputs[fieldTask].SetValue("Add PDF receipts")
	path := filepath.FromSlash("/src/shop-receipts")
	_, cmd := m.Update(createdMsg{path: path, branch: "feature/receipts", c: c})
	pump(t, m, cmd)
	s := m.sessionFor(path)
	if s == nil || len(fa.reqs) != 1 || fa.reqs[0].Message != "Add PDF receipts" || fa.reqs[0].Dir != path {
		t.Fatalf("task didn't start the agent: session=%v reqs=%+v", s, fa.reqs)
	}
	if m.focus != focusChat || m.chatPath != path {
		t.Error("the new worktree's agent should be on screen")
	}
}

func TestSuggestedCommands(t *testing.T) {
	reply := "Here:\n```grove-run\ngit add -A\n# comment\ngit commit -m \"x\"\n```\nand\n```bash\nrm -rf /\n```\n```grove-run\r\ngit push\r\n```"
	got := suggestedCommands(reply)
	want := []string{"git add -A", `git commit -m "x"`, "git push"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("suggestedCommands = %q, want %q", got, want)
	}
}
