package ui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/plan"
)

func TestHeaderKeepsBase(t *testing.T) {
	for _, w := range []int{60, 70, 90} {
		m := fakeModel(t, w, 24)
		head := strings.Split(plain(m), "\n")[0]
		if !strings.Contains(head, "changes since main") {
			t.Errorf("width %d: header lost the base branch: %q", w, head)
		}
	}
	m := fakeModel(t, 160, 24)
	head := strings.Split(plain(m), "\n")[0]
	for _, want := range []string{"test", "3 worktrees", "live"} {
		if !strings.Contains(head, want) {
			t.Errorf("wide header is missing %q: %q", want, head)
		}
	}
}

func TestDashboardIsOneLine(t *testing.T) {
	m := fakeModel(t, 120, 30)
	if m.dashHeight() != 1 {
		t.Fatalf("dashHeight = %d", m.dashHeight())
	}
	lines := leftColumn(m)
	if !strings.Contains(lines[1], "w overview") || !strings.Contains(lines[2], "Worktrees") {
		t.Errorf("summary line then the list expected:\n%s", strings.Join(lines[:4], "\n"))
	}
	// What the header shows already isn't repeated.
	if strings.Contains(lines[1], "worktrees") || strings.Contains(lines[1], "overlap") {
		t.Errorf("summary repeats the header: %q", lines[1])
	}
}

func TestWorktreeSummary(t *testing.T) {
	m := fakeModel(t, 140, 44)
	auth := filepath.FromSlash("/src/shop-auth")
	st := &m.snap.States[1]
	now := time.Now()
	st.Recent = []git.Commit{
		{Hash: "abc1234", Subject: "Retry API calls", When: now},
		{Hash: "bbb2222", Subject: "Second", When: now},
		{Hash: "ccc3333", Subject: "Third", When: now},
		{Hash: "ddd4444", Subject: "Fourth", When: now},
		{Hash: "eee5555", Subject: "Release 1.2", When: now},
	}
	s := m.newSession(auth, "feature/auth", agent.Claude)
	s.entries = []chatEntry{{roleAgent, "Added the **remember me** box."}}
	s.pending = []pendingAction{{kind: pendingApproval, text: "npm test"}}
	m.plans = []*plan.Plan{{ID: "p", Title: "Login", Status: plan.Running, Tasks: []plan.Task{
		{ID: "a", Title: "Remember me", Branch: "feature/auth", Path: auth, Status: plan.Working, Brief: "Add a remember-me checkbox."},
	}}}
	m.renderedFor = ""
	m.refreshDiffView()
	out := plain(m)
	for _, want := range []string{
		"Task", "Remember me", "Add a remember-me checkbox.",
		"Agent", "Claude Code", "needs you", "Added the remember me box.",
		"Recent commits", "↑ abc1234 Retry API calls", "↑ ddd4444 Fourth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary is missing %q:\n%s", want, out)
		}
	}
	// Ahead is 4: the fifth commit is already in main.
	if strings.Contains(out, "↑ eee5555") || !strings.Contains(out, "eee5555 Release 1.2") {
		t.Errorf("commit in the base marked ahead:\n%s", out)
	}
}

func TestFirstRun(t *testing.T) {
	m := fakeModel(t, 120, 30)
	if strings.Contains(plain(m), "Get started") {
		t.Error("Get started shown with three worktrees")
	}
	snap := *m.snap
	snap.States = snap.States[:1]
	snap.Overlaps = nil
	m.Update(scanMsg{seq: m.scanSeq, snap: &snap})
	out := plain(m)
	for _, want := range []string{"Get started", "New worktree", "Plan a feature", "Every key"} {
		if !strings.Contains(out, want) {
			t.Errorf("first run is missing %q:\n%s", want, out)
		}
	}
	lines := strings.Split(out, "\n")
	foot := lines[len(lines)-1]
	if !strings.Contains(foot, "n new worktree") || strings.Contains(foot, "merge") || strings.Contains(foot, "remove") {
		t.Errorf("first-run footer: %q", foot)
	}
	// The steps are text, not rows: the cursor stays on the worktree.
	press(m, "down", "down")
	if r, ok := m.selected(); !ok || r.kind != rowTree {
		t.Errorf("cursor left the worktree: %+v", r)
	}
}
