package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/git"
)

// until runs cmd and feeds the merge review's results back into the model
// until done reports true. Ticks and toasts are never waited for.
func until(t *testing.T, m *Model, cmd tea.Cmd, done func() bool) {
	t.Helper()
	results := make(chan tea.Msg, 64)
	start := func(c tea.Cmd) {
		if c != nil {
			go func() { results <- c() }()
		}
	}
	start(cmd)
	deadline := time.After(30 * time.Second)
	for !done() {
		select {
		case msg := <-results:
			switch msg := msg.(type) {
			case tea.BatchMsg:
				for _, c := range msg {
					start(c)
				}
			case conflictCheckMsg, mergePlanMsg, mergeDoneMsg, mergeAbortMsg:
				_, next := m.Update(msg)
				start(next)
			}
		case <-deadline:
			t.Fatal("until: still waiting after 30s")
		}
	}
}

// mergeModel builds a real repository: main, plus feature/a and feature/b,
// which change the same line differently, and feature/c, which adds a file.
// The model runs in main.
func mergeModel(t *testing.T) (*Model, string) {
	t.Helper()
	if git.Available() != nil {
		t.Skip("git not installed")
	}
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
	} {
		t.Setenv(k, v)
	}
	root := t.TempDir()
	mainDir := filepath.Join(root, "shop")
	g := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	g(mainDir, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(mainDir, "shared.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	g(mainDir, "add", ".")
	g(mainDir, "commit", "-qm", "init")
	for _, b := range []struct{ name, file, content string }{
		{"feature/a", "shared.txt", "one\nTWO from a\nthree\n"},
		{"feature/b", "shared.txt", "one\ntwo from b\nthree\n"},
		{"feature/c", "c.txt", "c\n"},
	} {
		dir := filepath.Join(root, "shop-"+strings.TrimPrefix(b.name, "feature/"))
		g(mainDir, "worktree", "add", "-q", dir, "-b", b.name)
		os.WriteFile(filepath.Join(dir, b.file), []byte(b.content), 0o644)
		g(dir, "add", ".")
		g(dir, "commit", "-qm", b.name)
	}

	ctx := context.Background()
	repo, err := git.Open(ctx, mainDir)
	if err != nil {
		t.Fatal(err)
	}
	m := New(ctx, Options{Repo: repo, Config: config.Default(), Version: "test"})
	noAgents(m)
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m.Update(baseMsg{base: "main"})
	rescan(t, m)
	// git's spelling of the folder, which grove uses everywhere: the temp
	// folder can differ (/var vs /private/var on macOS, short 8.3 names on
	// Windows).
	return m, repo.Here
}

func rescan(t *testing.T, m *Model) {
	t.Helper()
	snap, err := git.Scan(context.Background(), m.repo, "main", git.ModeBranch)
	if err != nil {
		t.Fatal(err)
	}
	m.scanning = false
	m.Update(scanMsg{seq: m.scanSeq, snap: snap})
}

func TestMergeReviewEndToEnd(t *testing.T) {
	m, mainDir := mergeModel(t)
	ctx := context.Background()
	if r, _ := m.selected(); m.snap.States[r.wt].Branch != "main" {
		t.Fatalf("should start on main, got %s", m.snap.States[r.wt].Branch)
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	until(t, m, cmd, func() bool { return m.conflicts.stage != stageChecking })
	c := m.conflicts
	if c.stage != stageChoose || len(c.checks) != 3 {
		t.Fatalf("stage %d with %d checks, want the 3 feature branches", c.stage, len(c.checks))
	}
	for _, ch := range c.checks {
		if ch.err != nil || ch.commits != 1 || len(ch.paths) != 0 {
			t.Errorf("%s alone should merge cleanly into main: %+v", ch.branch, ch)
		}
	}

	// Nothing is chosen until the user chooses.
	press(m, "enter")
	if c.stage != stageChoose {
		t.Fatal("enter with nothing chosen should not preview")
	}
	press(m, "a")
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	until(t, m, cmd, func() bool { return c.stage != stagePlanning })
	if c.stage != stageConfirm {
		t.Fatalf("stage = %d, want the confirm step", c.stage)
	}
	// a and b are clean on their own but conflict with each other, and the
	// preview, in merge order, must say so before anything changes.
	out := plain(m)
	if !strings.Contains(out, "the merge stops here") || !strings.Contains(out, "shared.txt") {
		t.Errorf("the plan should show the conflict between a and b:\n%s", out)
	}
	if busy, _ := git.MergeInProgress(ctx, mainDir); busy {
		t.Fatal("previewing must not start a merge")
	}

	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	until(t, m, cmd, func() bool { return c.stage != stageMerging })
	if c.stage != stageConflict || !slices.Equal(c.conflicted, []string{"shared.txt"}) {
		t.Fatalf("stage %d, conflicted %v; want a conflict in shared.txt", c.stage, c.conflicted)
	}
	if !slices.Equal(c.merged, []string{"feature/a"}) || c.stopped != "feature/b" || !slices.Equal(c.remaining, []string{"feature/c"}) {
		t.Errorf("merged %v, stopped at %q, remaining %v", c.merged, c.stopped, c.remaining)
	}

	// Asking the agent hands it the conflict, only once the user presses a.
	fake := &fakeAgent{}
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	m.runAgent = fake.run
	pump(t, m, func() tea.Msg { _, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"}); return cmd() })
	if m.conflicts != nil || m.focus != focusChat {
		t.Fatal("a should close the review and open the agent")
	}
	if len(fake.reqs) != 1 || !strings.Contains(fake.reqs[0].Message, "shared.txt") || !strings.Contains(fake.reqs[0].Message, "Do not commit") {
		t.Fatalf("agent request = %+v", fake.reqs)
	}
	if !git.SamePath(fake.reqs[0].Dir, mainDir) {
		t.Errorf("the agent should work in the target, got %s", fake.reqs[0].Dir)
	}

	// Opening the review again on a worktree mid-merge goes to the conflict.
	press(m, "tab")
	rescan(t, m)
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	until(t, m, cmd, func() bool { return m.conflicts.stage != stageChecking })
	if m.conflicts.stage != stageConflict {
		t.Fatalf("stage = %d, want the conflict", m.conflicts.stage)
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	until(t, m, cmd, func() bool { return m.conflicts == nil })
	if busy, _ := git.MergeInProgress(ctx, mainDir); busy {
		t.Error("x should abort the merge")
	}
	if n, err := git.CommitsBetween(ctx, mainDir, "main", "feature/a"); err != nil || n != 0 {
		t.Errorf("feature/a should stay merged after aborting b: %d commits missing, %v", n, err)
	}
}

func TestMergeReviewRefusesUncommittedTarget(t *testing.T) {
	m, mainDir := mergeModel(t)
	if err := os.WriteFile(filepath.Join(mainDir, "shared.txt"), []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	until(t, m, cmd, func() bool { return m.conflicts.stage != stageChecking })
	press(m, "space")
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	until(t, m, cmd, func() bool { return m.conflicts.stage != stagePlanning })
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	until(t, m, cmd, func() bool { return m.conflicts == nil })
	if !strings.Contains(m.toast.text, "uncommitted changes") {
		t.Errorf("toast = %q, want a refusal", m.toast.text)
	}
	if b, _ := os.ReadFile(filepath.Join(mainDir, "shared.txt")); string(b) != "local edit\n" {
		t.Errorf("the user's edit was touched: %q", b)
	}
}

// The review keeps worktrees by path, so a scan that drops or reorders
// worktrees while it is open can't make it show or merge the wrong one.
func TestMergeReviewSurvivesRescan(t *testing.T) {
	m := fakeModel(t, 120, 36)
	press(m, "c")
	m.Update(conflictCheckMsg{seq: m.conflicts.seq, checks: []mergeCheck{{name: "feature/cart", branch: "feature/cart", commits: 2}}})
	snap := *m.snap
	snap.States = snap.States[:1]
	m.Update(scanMsg{seq: m.scanSeq, snap: &snap})
	out := plain(m) // used to index past the end of States
	if !strings.Contains(out, "Merge branches into feature/auth") || !strings.Contains(out, "feature/cart") {
		t.Errorf("review lost its worktrees after a rescan:\n%s", out)
	}
	m.Update(tea.MouseClickMsg{X: 5, Y: 10, Button: tea.MouseLeft})
	if m.conflicts == nil {
		t.Error("a click behind the review should be ignored, not close or bypass it")
	}
}
