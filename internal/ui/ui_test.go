package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/gh"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/store"
	"github.com/charmbracelet/x/ansi"
)

// fakeModel builds a model around a hand-made snapshot so the UI can be
// exercised without git.
func fakeModel(t *testing.T, w, h int) *Model {
	t.Helper()
	repo := &git.Repo{Here: filepath.FromSlash("/src/shop-auth"), Name: "shop"}
	m := New(context.Background(), Options{Repo: repo, Config: config.Default(), Version: "test"})
	noAgents(m)
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.Update(baseMsg{base: "main"})
	now := time.Now()
	snap := &git.Snapshot{
		Base: "main",
		States: []git.State{
			{Worktree: git.Worktree{Path: filepath.FromSlash("/src/shop"), Branch: "main", Main: true}, Base: "main", Rev: "HEAD"},
			{
				Worktree: git.Worktree{Path: filepath.FromSlash("/src/shop-auth"), Branch: "feature/auth"},
				Base:     "main", Rev: "abc1234", Ahead: 4, Behind: 1,
				Last: &git.Commit{Hash: "abc1234", Subject: "Retry API calls", Author: "Dev", When: now.Add(-2 * time.Hour)},
				Files: []git.FileChange{
					{Path: "src/api/client.ts", Status: git.Modified, Added: 20, Deleted: 4},
					{Path: "src/auth/login.ts", Status: git.Modified, Added: 88, Deleted: 12, Uncommitted: true},
					{Path: "src/auth/oauth.ts", Status: git.Untracked, Added: 14, Uncommitted: true},
					{Path: "assets/logo.png", Status: git.Added, Binary: true},
				},
				Added: 122, Deleted: 16,
			},
			{
				Worktree: git.Worktree{Path: filepath.FromSlash("/src/shop-cart"), Branch: "feature/cart"},
				Base:     "main", Rev: "def5678", Ahead: 2,
				Files: []git.FileChange{
					{Path: "src/api/client.ts", Status: git.Modified, Added: 8, Deleted: 2},
					{Path: "src/cart/old.ts", OldPath: "src/cart/cart.ts", Status: git.Renamed, Added: 1, Deleted: 1},
				},
				Added: 9, Deleted: 3,
			},
		},
		Overlaps: map[string][]int{"src/api/client.ts": {1, 2}},
		Taken:    now,
	}
	m.Update(scanMsg{seq: m.scanSeq, snap: snap})
	return m
}

func plain(m *Model) string { return ansi.Strip(m.render()) }

// noAgents makes a model independent of the AI tools and editors on this
// machine. Without clearing the editor, tests that create a worktree would
// launch VS Code for real.
func noAgents(m *Model) {
	m.opener.Editor = ""
	m.installed = func() []agent.Kind { return nil }
	m.saveAgent = func(string) error { return nil }
	m.saveModel = func(string, string) error { return nil }
	m.saveWatch = func(string) error { return nil }
	// As if the config answered who watches plans; tests of the question
	// clear it.
	m.cfg.Planner.Watcher = config.WatchYou
	m.runAgent = func(_ context.Context, _ agent.Request, emit func(agent.Event)) {
		emit(agent.Event{Kind: agent.EvDone, Err: fmt.Errorf("no agent in tests")})
	}
	m.warmAgent = func(agent.Request) {}
	m.stopAgent = func(string) {}
	m.sendAlert = func(string, string) error { return nil }
	m.ghPath = ""
	m.listPRs = func(context.Context, string) (map[string]gh.PR, error) { return nil, nil }
	m.prHooks.push = func(context.Context, string, string, string, []string) error {
		return fmt.Errorf("no pushing in tests")
	}
	m.prHooks.open = func(context.Context, string, string) (gh.PR, bool, error) { return gh.PR{}, false, nil }
	m.prHooks.create = func(context.Context, string, string, string, string, string) (string, error) {
		return "", fmt.Errorf("no pull requests in tests")
	}
}

func press(m *Model, keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "up":
			msg = tea.KeyPressMsg{Code: tea.KeyUp}
		case "left":
			msg = tea.KeyPressMsg{Code: tea.KeyLeft}
		case "right":
			msg = tea.KeyPressMsg{Code: tea.KeyRight}
		case "shift+tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
		case "ctrl+k":
			msg = tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl}
		default:
			r := []rune(k)[0]
			msg = tea.KeyPressMsg{Code: r, Text: k}
		}
		m.Update(msg)
	}
}

func TestRenderFitsScreen(t *testing.T) {
	for _, size := range [][2]int{{160, 40}, {120, 30}, {100, 24}, {80, 24}, {60, 14}, {40, 10}} {
		m := fakeModel(t, size[0], size[1])
		for _, screen := range []string{"list", "diff", "help", "remove", "create", "agent", "model", "picker", "overview", "merge", "merge-plan", "merge-conflict", "planner", "review", "review-edit", "review-problems", "group", "plan-chat", "group-list", "watch", "overview-conflicts", "diff-conflicts", "actions", "actions-filtered", "ship", "commit", "worktree-view", "worktree-search", "revert"} {
			switch screen {
			case "diff":
				press(m, "down", "tab")
			case "help":
				press(m, "?")
			case "actions":
				m = fakeModel(t, size[0], size[1])
				press(m, "ctrl+k")
			case "actions-filtered":
				typeText(m, "zzz")
			case "ship":
				m = fakeModel(t, size[0], size[1])
				press(m, "s")
				m.Update(shipLoadedMsg{path: m.ship.path, up: git.Upstream{Name: "origin/" + strings.Repeat("long-branch/", 8), Remote: "origin", Ahead: 3, Behind: 2}})
			case "worktree-view":
				m = fakeModel(t, size[0], size[1])
				press(m, "v")
				long := strings.Repeat("deep/", 30) + "file.ts"
				raw := reviewDiff + "diff --git a/" + long + " b/" + long + "\n--- a/" + long + "\n+++ b/" + long +
					"\n@@ -1 +1 @@\n-" + strings.Repeat("x", 300) + "\n+y\n"
				m.Update(wtDiffMsg{key: m.wtView.loading, raw: raw})
			case "worktree-search":
				press(m, "/")
				typeText(m, strings.Repeat("hello ", 30))
			case "revert":
				press(m, "esc", "x")
			case "commit":
				press(m, "c")
				m.commit.message.SetValue(strings.Repeat("A long commit message line. ", 20))
			case "remove":
				m = fakeModel(t, size[0], size[1])
				press(m, "d", "enter", "b")
			case "create":
				m = fakeModel(t, size[0], size[1])
				press(m, "n")
				typeText(m, "feature/a-much-longer-branch-name-than-usual")
				press(m, "enter")
			case "agent":
				m = fakeModel(t, size[0], size[1])
				m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
				press(m, "a")
				s := m.chatSession()
				s.entries = []chatEntry{
					{roleUser, strings.Repeat("please change a lot of things ", 8)},
					{roleAgent, "Done.\n\n```grove-run\ngit commit -m \"" + strings.Repeat("long message ", 12) + "\"\n```"},
					{roleTool, "Edit " + strings.Repeat("deep/", 30) + "file.ts"},
				}
				s.pending = []pendingAction{{kind: pendingApproval, text: strings.Repeat("npm run test -- --long-flag ", 6), tool: "Bash"}}
			case "model":
				s := m.chatSession()
				s.pending = nil
				s.model = "a-custom-model-with-a-rather-long-name"
				m.modelCommand(s, "")
			case "picker":
				m = fakeModel(t, size[0], size[1])
				m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude, agent.Codex} }
				press(m, "a")
			case "overview":
				m = fakeModel(t, size[0], size[1])
				press(m, "w")
			case "merge":
				m = fakeModel(t, size[0], size[1])
				press(m, "c")
				long := strings.Repeat("deep/", 20) + "file.ts"
				var checks []mergeCheck
				for i := range 30 {
					checks = append(checks, mergeCheck{name: fmt.Sprintf("feature/branch-with-a-long-name-%d", i), branch: "b", commits: 3, paths: []string{long, long}, dirty: true})
				}
				m.Update(conflictCheckMsg{seq: m.conflicts.seq, checks: checks})
			case "merge-plan":
				c := m.conflicts
				for i := range c.checks {
					c.checks[i].selected = true
				}
				c.stage = stageConfirm
				c.plan = []git.MergeStep{{Source: "b"}, {Source: "b", Conflicts: c.checks[0].paths}}
			case "merge-conflict":
				c := m.conflicts
				c.stage, c.stopped, c.conflicted = stageConflict, c.checks[1].name, nil
				for i := range 40 {
					c.conflicted = append(c.conflicted, fmt.Sprintf("%s-%d", c.checks[0].paths[0], i))
				}
				c.merged = []string{c.checks[0].name}
				c.remaining = []string{c.checks[2].name, c.checks[3].name}
			case "planner":
				m = fakeModel(t, size[0], size[1])
				m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
				press(m, "P")
			case "review":
				long := strings.Repeat("A very long task title ", 6)
				reply := "```grove-plan\n{\"title\": \"" + long + "\", \"groups\": [{\"id\": \"g\", \"name\": \"" + long + "\"}], \"tasks\": ["
				for i := range 25 {
					if i > 0 {
						reply += ","
					}
					reply += fmt.Sprintf(`{"title": "%s %d", "branch": "feature/%s-%d", "group": "g", "brief": "%s"}`, long, i, strings.Repeat("x", 80), i, strings.Repeat("Do this and that. ", 40))
				}
				reply += "]}\n```"
				m.onPlannerReply(m.planner, reply)
				m.review = newReview(m.draft())
			case "review-edit":
				press(m, "down", "enter")
			case "group":
				m = fakeModel(t, size[0], size[1])
				m.store = store.Open(t.TempDir())
				p := &plan.Plan{ID: "p", Title: strings.Repeat("Long plan title ", 8), Status: plan.Running,
					Groups: []plan.Group{{ID: "g", Name: strings.Repeat("Long group ", 6)}},
					Tasks: []plan.Task{
						{ID: "a", Branch: "feature/auth", Path: filepath.FromSlash("/src/shop-auth"), Group: "g", Status: plan.Working},
						{ID: "c", Branch: "feature/cart", Path: filepath.FromSlash("/src/shop-cart"), Group: "g", Status: plan.Finished},
					}}
				m.plans = []*plan.Plan{p}
				m.rebuildRows()
				log, _ := m.groupLog(p, "g")
				for i := range 30 {
					log.Post(group.Message{From: "feature/auth", To: "feature/cart", Text: fmt.Sprintf("%d %s", i, strings.Repeat("a long message ", 20))})
				}
				m.openGroupChat(filepath.FromSlash("/src/shop-auth"))
				m.pollingGroups = false
				m.Update(m.pollGroups()())
				m.groups[log.Dir].paused = true
			case "plan-chat":
				m = fakeModel(t, size[0], size[1])
				m.store = store.Open(t.TempDir())
				p := &plan.Plan{ID: "p", Title: strings.Repeat("Long plan title ", 8), Status: plan.Review,
					Merge: &plan.Merge{Branch: "plan/" + strings.Repeat("long-", 20), Path: filepath.FromSlash("/src/" + strings.Repeat("long-", 20))},
					Tasks: []plan.Task{{ID: "a", Branch: "feature/auth", Path: filepath.FromSlash("/src/shop-auth"), Status: plan.Finished}}}
				m.plans = []*plan.Plan{p}
				log, _ := m.groupLog(p, plan.ChatID)
				log.Post(group.Message{From: group.Planner, Text: strings.Repeat("Merged and tested. ", 30)})
				m.openGroupChat(filepath.FromSlash("/src/shop-auth"))
				m.pollingGroups = false
				m.Update(m.pollGroups()())
			case "group-list":
				press(m, "tab")
				m.moveTo(m.groupRow(0))
				m.refreshDiffView()
			case "watch":
				m = fakeModel(t, size[0], size[1])
				m.cfg.Planner.Watcher = ""
				m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
				press(m, "P", "down")
			case "overview-conflicts":
				m = fakeModel(t, size[0], size[1])
				// Many pairs, so the Conflicts section has to stop early.
				snap := *m.snap
				for i := range 8 {
					snap.States = append(snap.States, git.State{
						Worktree: git.Worktree{Path: filepath.FromSlash(fmt.Sprintf("/src/shop-%d", i)), Branch: fmt.Sprintf("feature/a-rather-long-branch-name-%d", i)},
						Files:    []git.FileChange{{Path: "src/api/client.ts", Status: git.Modified}},
					})
				}
				snap.Overlaps = map[string][]int{"src/api/client.ts": {1, 2, 3, 4, 5, 6, 7, 8, 9, 10}}
				m.Update(scanMsg{seq: m.scanSeq, snap: &snap})
				var preds []git.Prediction
				for a := 1; a <= 10; a++ {
					for b := a + 1; b <= 10; b++ {
						preds = append(preds, git.Prediction{A: a, B: b, Files: []git.FileConflict{{Path: "src/api/client.ts"}, {Path: strings.Repeat("deep/", 20) + "x.ts"}}})
					}
				}
				predicted(m, preds)
				press(m, "w")
			case "diff-conflicts":
				press(m, "w", "down", "tab")
				m.Update(diffMsg{key: m.diffWanted, raw: conflictDiff})
			case "review-problems":
				press(m, "esc")
				for i := range 12 {
					m.review.problems = append(m.review.problems, fmt.Sprintf("%s problem %d", strings.Repeat("very ", 30), i))
				}
			}
			out := m.render()
			lines := strings.Split(out, "\n")
			if len(lines) != size[1] && size[0] >= minWidth && size[1] >= minHeight {
				t.Errorf("%dx%d %s: %d lines, want %d", size[0], size[1], screen, len(lines), size[1])
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > size[0] {
					t.Errorf("%dx%d %s: line %d is %d wide, want <= %d:\n%s", size[0], size[1], screen, i, w, size[0], ansi.Strip(l))
				}
			}
		}
	}
}

func TestStartsOnCurrentWorktreeAndShowsOverlap(t *testing.T) {
	m := fakeModel(t, 140, 36)
	r, ok := m.selected()
	if !ok || r.kind != rowTree || m.snap.States[r.wt].Branch != "feature/auth" {
		t.Fatalf("selection should start on the worktree grove runs in, got %+v", r)
	}
	out := plain(m)
	for _, want := range []string{"feature/auth", "feature/cart", "◆ here", "⚠ 1 overlap", "↑4", "↓1", "src/api/client.ts", "grove  test  shop"} {
		if !strings.Contains(out, want) {
			t.Errorf("screen is missing %q", want)
		}
	}
	press(m, "down") // src/api/client.ts in feature/auth
	if out := plain(m); !strings.Contains(out, "Also changed in feature/cart") {
		t.Errorf("overlap banner missing:\n%s", out)
	}
}

func TestFilterAndOverlapsOnly(t *testing.T) {
	m := fakeModel(t, 140, 36)
	press(m, "/", "o", "a", "u", "t", "h")
	if m.filterQuery != "oauth" {
		t.Fatalf("filter = %q", m.filterQuery)
	}
	if n := m.fileRowCount(); n != 1 {
		t.Errorf("filter matched %d files, want 1", n)
	}
	press(m, "esc")
	if m.filterQuery != "" || m.fileRowCount() != 6 {
		t.Errorf("esc should clear the filter, have %q and %d files", m.filterQuery, m.fileRowCount())
	}
	press(m, "o")
	if n := m.fileRowCount(); n != 2 {
		t.Errorf("overlaps only shows %d files, want 2", n)
	}
}

func TestCollapseKeepsSelection(t *testing.T) {
	m := fakeModel(t, 140, 36)
	press(m, "down", "down", "left") // into login.ts, then back to the header
	r, _ := m.selected()
	if r.kind != rowTree {
		t.Fatalf("left on a file should select its worktree, got %+v", r)
	}
	press(m, "left")
	if !m.collapsed[m.snap.States[r.wt].Path] {
		t.Fatal("left on a header should collapse it")
	}
	if r2, _ := m.selected(); r2.kind != rowTree || r2.wt != r.wt {
		t.Errorf("selection moved after collapsing: %+v", r2)
	}
	if strings.Contains(plain(m), "src/auth/login.ts") {
		t.Error("collapsed worktree still lists files")
	}
}

func TestRefreshKeepsSelectedFile(t *testing.T) {
	m := fakeModel(t, 140, 36)
	press(m, "down", "down")
	_, before, _ := m.selectedFile()
	snap := *m.snap
	snap.States = append([]git.State(nil), snap.States...)
	// A new file appears above the selection.
	st := snap.States[1]
	st.Files = append([]git.FileChange{{Path: "a-first.go", Status: git.Untracked, Added: 1}}, st.Files...)
	snap.States[1] = st
	m.Update(scanMsg{seq: m.scanSeq, snap: &snap})
	_, after, _ := m.selectedFile()
	if after.Path != before.Path {
		t.Errorf("selection jumped from %s to %s", before.Path, after.Path)
	}
}

func TestRemoveDialog(t *testing.T) {
	m := fakeModel(t, 140, 36)

	press(m, "g", "d") // main
	if m.confirm != nil {
		t.Fatal("the main worktree must not be offered for removal")
	}

	// feature/auth has uncommitted files: enter alone must not remove it.
	press(m, "]", "d")
	if m.confirm == nil || m.confirm.st.Branch != "feature/auth" {
		t.Fatalf("dialog = %+v", m.confirm)
	}
	out := plain(m)
	for _, want := range []string{"Remove worktree", "2 files with uncommitted changes", "Discard uncommitted changes", "4 commits not in main"} {
		if !strings.Contains(out, want) {
			t.Errorf("dialog is missing %q:\n%s", want, out)
		}
	}
	press(m, "enter")
	if m.removing != nil || !m.confirm.nudged {
		t.Fatal("enter without discarding should only nudge")
	}
	press(m, "f", "b", "enter")
	if m.confirm != nil || m.removing == nil || !m.removing.force || !m.removing.deleteBranch {
		t.Fatalf("confirm didn't start the removal: confirm=%+v removing=%+v", m.confirm, m.removing)
	}
	if m.startScan() != nil {
		t.Error("scans should pause while a worktree is being removed")
	}
	if !strings.Contains(plain(m), "removing feature/auth") {
		t.Error("header should show the removal in progress")
	}

	m.Update(removeMsg{r: m.removing})
	if m.removing != nil || m.repo.Here != "" || !strings.Contains(m.toast.text, "deleted its branch") {
		t.Errorf("after removal: removing=%v here=%q toast=%q", m.removing, m.repo.Here, m.toast.text)
	}

	// A clean worktree needs no extra box, and esc backs out.
	press(m, "G", "d")
	if m.confirm == nil || m.confirm.needsForce() {
		t.Fatalf("clean worktree dialog = %+v", m.confirm)
	}
	press(m, "esc")
	if m.confirm != nil {
		t.Error("esc should close the dialog")
	}
}

func TestGitPrompt(t *testing.T) {
	m := fakeModel(t, 140, 36)
	press(m, "]", ":") // feature/cart
	if !m.gitCmd.active || m.gitCmd.target.Branch != "feature/cart" {
		t.Fatalf("prompt should target feature/cart: %+v", m.gitCmd.target.Worktree)
	}
	press(m, "g", "i", "t", " ", "s", "t", "a", "t", "u", "s")
	if out := plain(m); !strings.Contains(out, "feature/cart  git git status") {
		t.Errorf("footer doesn't show the prompt:\n%s", out)
	}
	press(m, "enter")
	if m.gitCmd.active || len(m.gitCmd.history) != 1 || m.gitCmd.history[0] != "status" {
		t.Fatalf("after enter: active=%v history=%q", m.gitCmd.active, m.gitCmd.history)
	}

	press(m, ":", "up")
	if v := m.gitCmd.input.Value(); v != "status" {
		t.Errorf("up should recall the last command, got %q", v)
	}
	press(m, "esc")
	if m.gitCmd.active {
		t.Error("esc should close the prompt")
	}
	press(m, "q")
	if m.gitCmd.active {
		t.Error("q after closing the prompt should not reopen it")
	}
}

func TestCreateDialog(t *testing.T) {
	m := fakeModel(t, 140, 36)
	press(m, "n")
	c := m.creating
	if c == nil {
		t.Fatal("n should open the new-worktree dialog")
	}
	m.Update(branchesMsg{b: git.Branches{
		Local:  map[string]bool{"main": true, "feature/auth": true, "feature/cart": true, "spare": true},
		Remote: map[string]bool{"origin/feature/remote": true},
	}})
	if got := c.inputs[fieldFrom].Value(); got != "main" {
		t.Errorf("From should default to the base branch, got %q", got)
	}

	typeText(m, "feature/receipts")
	if got, want := c.inputs[fieldFolder].Value(), filepath.FromSlash("/src/shop-feature-receipts"); got != want {
		t.Errorf("suggested folder = %q, want %q", got, want)
	}
	if text, ok := m.branchStatus(); !ok || text != "new branch" {
		t.Errorf("status = %q, %v", text, ok)
	}

	// A branch that another worktree has checked out can't be used.
	clearInput(m, fieldBranch)
	typeText(m, "feature/auth")
	if text, ok := m.branchStatus(); ok || !strings.Contains(text, "checked out in feature/auth") {
		t.Errorf("status = %q, %v", text, ok)
	}
	press(m, "enter")
	if c.running || !strings.Contains(c.err, "checked out") {
		t.Errorf("enter on a bad branch should explain, not create: running=%v err=%q", c.running, c.err)
	}

	// Existing and remote branches skip the From field.
	clearInput(m, fieldBranch)
	typeText(m, "feature/remote")
	if text, _ := m.branchStatus(); text != "new local branch tracking origin/feature/remote" {
		t.Errorf("remote status = %q", text)
	}
	for _, f := range m.fields() {
		if f == fieldFrom {
			t.Error("From should not be offered for an existing branch")
		}
	}

	// Typing a folder stops the suggestion from overwriting it.
	press(m, "tab")
	if c.focus != fieldFolder {
		t.Fatalf("tab should go to Folder, focus = %d", c.focus)
	}
	typeText(m, "-mine")
	press(m, "shift+tab")
	typeText(m, "x")
	if !strings.HasSuffix(c.inputs[fieldFolder].Value(), "-mine") {
		t.Errorf("custom folder was replaced: %q", c.inputs[fieldFolder].Value())
	}

	press(m, "esc")
	if m.creating != nil {
		t.Error("esc should close the dialog")
	}
}

// TestCreateEndToEnd creates a real worktree through the dialog.
func TestCreateEndToEnd(t *testing.T) {
	if git.Available() != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	mainDir := filepath.Join(root, "shop")
	gitc := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", mainDir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(mainDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitc("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(mainDir, "a.txt"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(mainDir, ".gitignore"), []byte(".env\n"), 0o644)
	gitc("add", ".")
	gitc("commit", "-qm", "init")
	os.WriteFile(filepath.Join(mainDir, ".env"), []byte("SECRET=1\n"), 0o644)

	ctx := context.Background()
	repo, err := git.Open(ctx, mainDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Editor = "none-for-test"
	m := New(ctx, Options{Repo: repo, Config: cfg, Version: "test"})
	noAgents(m)
	m.opener.Editor = ""
	m.Update(tea.WindowSizeMsg{Width: 140, Height: 36})
	m.Update(baseMsg{base: "main"})
	scan := func() {
		snap, err := git.Scan(ctx, repo, "main", git.ModeBranch)
		if err != nil {
			t.Fatal(err)
		}
		m.Update(scanMsg{seq: m.scanSeq, snap: snap})
	}
	scan()

	press(m, "n")
	b, err := git.ListBranches(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	m.Update(branchesMsg{b: b})
	typeText(m, "feature/receipts")
	want := filepath.Join(filepath.Dir(m.creating.mainPath), "shop-feature-receipts")
	if got := m.creating.inputs[fieldFolder].Value(); !git.SamePath(got, want) {
		t.Fatalf("folder = %s, want %s", got, want)
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	var created *createdMsg
	for _, msg := range runCmds(cmd) {
		if c, ok := msg.(createdMsg); ok {
			created = &c
		}
	}
	if created == nil || created.err != nil {
		t.Fatalf("create = %+v", created)
	}
	m.Update(*created)
	if m.creating != nil || created.copied != 1 {
		t.Errorf("after create: dialog open=%v, copied=%d", m.creating != nil, created.copied)
	}
	if b, err := os.ReadFile(filepath.Join(created.path, ".env")); err != nil || string(b) != "SECRET=1\n" {
		t.Errorf(".env not copied: %q %v", b, err)
	}

	scan()
	r, ok := m.selected()
	if !ok || r.kind != rowTree || m.snap.States[r.wt].Branch != "feature/receipts" {
		t.Errorf("the new worktree should be selected after the scan, got %+v", r)
	}
}

// runCmds runs a command and any batch it returns, collecting the messages.
// Ticks are skipped so the test doesn't wait on animations.
func runCmds(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	var out []tea.Msg
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				out = append(out, runCmds(c)...)
			}
			return out
		}
		return append(out, msg)
	case <-time.After(30 * time.Second):
		return out
	}
}

func typeText(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func clearInput(m *Model, field int) {
	m.creating.inputs[field].SetValue("")
	m.syncFolder()
}

func TestLeftAtDiffEdgeReturnsToList(t *testing.T) {
	m := fakeModel(t, 140, 36)
	press(m, "down")
	st, f, _ := m.selectedFile()
	long := strings.Repeat("x", 300)
	m.Update(diffMsg{key: diffKey(st, f), raw: "@@ -1,1 +1,1 @@ x\n-" + long + "\n+" + long + "y\n"})

	press(m, "right") // a file row: into the diff
	if m.focus != focusDiff {
		t.Fatal("right on a file should focus the diff")
	}
	press(m, "right", "right", "left")
	if m.focus != focusDiff || m.diff.XOffset() == 0 {
		t.Fatalf("left while scrolled should scroll, not leave: focus=%v x=%d", m.focus, m.diff.XOffset())
	}
	for i := 0; i < 10 && m.diff.XOffset() > 0; i++ {
		press(m, "left")
	}
	if m.focus != focusDiff {
		t.Fatal("reaching the left edge should not leave the diff by itself")
	}
	press(m, "left")
	if m.focus != focusList {
		t.Error("left at the left edge should move to the file list")
	}
}

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`status`:                         {"status"},
		`commit -m "fix login bug"`:      {"commit", "-m", "fix login bug"},
		`commit -m 'it''s'`:              {"commit", "-m", "its"},
		`log  --oneline   -5`:            {"log", "--oneline", "-5"},
		`commit -m "say 'hi'" --no-edit`: {"commit", "-m", "say 'hi'", "--no-edit"},
		`commit -m ""`:                   {"commit", "-m", ""},
	}
	for in, want := range cases {
		if got := splitCommand(in); strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("splitCommand(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestGitRun(t *testing.T) {
	if git.Available() != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	runSteps := func(steps ...termStep) (*gitRun, string) {
		var out strings.Builder
		g := &gitRun{dir: dir, steps: steps, st: newStyles(true)}
		g.SetStdin(strings.NewReader("\n"))
		g.SetStdout(&out)
		g.SetStderr(&out)
		if err := g.Run(); err != nil {
			t.Fatal(err)
		}
		return g, ansi.Strip(out.String())
	}
	run := func(args ...string) (*gitRun, string) {
		return runSteps(termStep{args: args, display: "git " + strings.Join(args, " ")})
	}
	g, out := run("status", "--short", "--branch")
	if g.code != 0 || !strings.Contains(out, "##") || !strings.Contains(out, "press enter") {
		t.Errorf("status: code %d, output:\n%s", g.code, out)
	}
	if g, out := run("not-a-command"); g.code == 0 || !strings.Contains(out, "exit") {
		t.Errorf("bad command: code %d, output:\n%s", g.code, out)
	}
	// Steps stop at the first failure.
	g, out = runSteps(
		termStep{args: []string{"status", "--short"}, display: "git status --short"},
		termStep{args: []string{"not-a-command"}, display: "git not-a-command"},
		termStep{args: []string{"log"}, display: "git log"},
	)
	if g.code == 0 || !strings.Contains(out, "$ git status --short") || !strings.Contains(out, "$ git not-a-command") || strings.Contains(out, "$ git log") {
		t.Errorf("steps: code %d, output:\n%s", g.code, out)
	}
}

func TestDiffRendering(t *testing.T) {
	raw := "diff --git a/x.go b/x.go\nindex 1..2 100644\n--- a/x.go\n+++ b/x.go\n" +
		"@@ -1,4 +1,4 @@ func main()\n package main\n-const timeout = 5000\n+const timeout = 10000\n \tfmt.Println()\n\\ No newline at end of file\n"
	pd := parseDiff(raw)
	if len(pd.lines) != 6 {
		t.Fatalf("got %d lines, want 6: %+v", len(pd.lines), pd.lines)
	}
	del, add := pd.lines[2], pd.lines[3]
	if del.old != 2 || add.new != 2 {
		t.Errorf("line numbers wrong: del %d add %d", del.old, add.new)
	}
	if del.hiEnd == 0 || add.text[add.hiStart:add.hiEnd] != "10000" {
		t.Errorf("word highlight = %q", add.text[add.hiStart:add.hiEnd])
	}
	content, gutter := renderDiff(pd, "x.go", 80, newStyles(true), nil, "")
	if len(content) != len(gutter) {
		t.Fatal("gutter and content out of step")
	}
	w := ansi.StringWidth(gutter[0])
	for i, g := range gutter {
		if ansi.StringWidth(g) != w {
			t.Errorf("gutter line %d is %d wide, want %d", i, ansi.StringWidth(g), w)
		}
		if got := ansi.StringWidth(content[i]); got != 80-w {
			t.Errorf("content line %d is %d wide, want %d", i, got, 80-w)
		}
	}
	if !strings.Contains(ansi.Strip(content[3]), "+ const timeout = 10000") {
		t.Errorf("added line rendered as %q", ansi.Strip(content[3]))
	}
}

func TestBinaryDiff(t *testing.T) {
	if pd := parseDiff("diff --git a/a.png b/a.png\nBinary files /dev/null and b/a.png differ\n"); !pd.binary {
		t.Error("binary diff not detected")
	}
}

// TestDump renders a real repository to text files for eyeballing. Run with
// GROVE_DEMO=<repo path> go test ./internal/ui -run Dump.
func TestDump(t *testing.T) {
	dir := os.Getenv("GROVE_DEMO")
	if dir == "" {
		t.Skip("set GROVE_DEMO to a repository path")
	}
	ctx := context.Background()
	repo, err := git.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("GROVE_DUMP")
	for _, size := range [][2]int{{150, 34}, {90, 28}} {
		m := New(ctx, Options{Repo: repo, Config: config.Default(), Version: "dev"})
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		base, _ := git.DetectBase(ctx, repo, "")
		m.Update(baseMsg{base: base})
		snap, err := git.Scan(ctx, repo, base, git.ModeBranch)
		if err != nil {
			t.Fatal(err)
		}
		m.Update(scanMsg{seq: m.scanSeq, snap: snap})
		shots := map[string]func(){
			"1-tree":  func() {},
			"2-file":  func() { press(m, "down") },
			"3-diff":  func() { press(m, "tab") },
			"4-help":  func() { press(m, "tab", "?") },
			"5-other": func() { press(m, "esc", "]", "down") },
		}
		for _, name := range []string{"1-tree", "2-file", "3-diff", "4-help", "5-other"} {
			shots[name]()
			// Let the diff load synchronously.
			if st, f, ok := m.selectedFile(); ok {
				raw, err := git.Diff(ctx, st, f)
				m.Update(diffMsg{key: diffKey(st, f), raw: raw, err: err})
			}
			p := filepath.Join(out, fmt.Sprintf("%s-%dx%d", name, size[0], size[1]))
			os.WriteFile(p+".txt", []byte(ansi.Strip(m.render())), 0o644)
			os.WriteFile(p+".ans", []byte(m.render()), 0o644)
		}
	}
}
