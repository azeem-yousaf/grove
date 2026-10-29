package ui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/usage"
	"github.com/charmbracelet/x/ansi"
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

// summary renders the selected worktree's summary.
func summary(m *Model) string {
	m.renderedFor = ""
	m.refreshDiffView()
	return plain(m)
}

func TestSummaryShowsAgentUsage(t *testing.T) {
	m := fakeModel(t, 140, 44)
	withLedger(t, m)

	// No session: no Usage line.
	if out := summary(m); strings.Contains(out, "Usage") {
		t.Errorf("Usage line without an agent:\n%s", out)
	}
	s := m.newSession(filepath.FromSlash("/src/shop-auth"), "feature/auth", agent.Claude)
	if out := summary(m); strings.Contains(out, "Usage") {
		t.Errorf("Usage line before any usage:\n%s", out)
	}

	m.usage.Add(usage.Turn{ID: s.uid + "/1", Agent: s.uid, Tokens: usage.Tokens{Input: 1200, Output: 300}, Status: usage.Exact})
	s.cost = "$0.12"
	out := summary(m)
	if !strings.Contains(out, "Usage") || !strings.Contains(out, "1.5k tok · $0.12") {
		t.Errorf("summary lacks tokens and cost:\n%s", out)
	}
	if strings.Contains(out, "before this turn") {
		t.Errorf("idle agent's cost marked as before this turn:\n%s", out)
	}

	// While a turn runs: live figures, and the cost from before it.
	m.startTurn(s)
	s.running = true
	s.live, s.liveSeen = usage.Tokens{Input: 2300}, true
	if out := summary(m); !strings.Contains(out, "1.5k tok + 2.3k live · $0.12 before this turn") {
		t.Errorf("running summary:\n%s", out)
	}
	// The live count follows the agent's events, as in the chat header.
	s.live = usage.Tokens{Input: 4100}
	out = summary(m)
	if !strings.Contains(out, "+ 4.1k live") {
		t.Errorf("live count not updated:\n%s", out)
	}
	if head := ansi.Strip(strings.Join(m.agentUsageCost(s), " · ")); !strings.Contains(out, head) {
		t.Errorf("summary differs from the chat header's %q:\n%s", head, out)
	}
}

func TestSummaryUsageCodexHasNoCost(t *testing.T) {
	m := fakeModel(t, 140, 44)
	withLedger(t, m)
	s := m.newSession(filepath.FromSlash("/src/shop-auth"), "feature/auth", agent.Codex)
	m.usage.Add(usage.Turn{ID: s.uid + "/1", Agent: s.uid, Tokens: usage.Tokens{Input: 1200, Output: 300}, Status: usage.Exact})
	out := summary(m)
	if !strings.Contains(out, "1.5k tok") {
		t.Errorf("Codex summary lacks tokens:\n%s", out)
	}
	if strings.Contains(out, "$") {
		t.Errorf("Codex summary shows a cost:\n%s", out)
	}
}

func TestSummaryUsageWithoutLedger(t *testing.T) {
	m := fakeModel(t, 140, 44)
	s := m.newSession(filepath.FromSlash("/src/shop-auth"), "feature/auth", agent.Claude)
	if out := summary(m); strings.Contains(out, "Usage") {
		t.Errorf("Usage line with no ledger and no cost:\n%s", out)
	}
	s.cost = "$0.40"
	if out := summary(m); !strings.Contains(out, "Usage") || !strings.Contains(out, "$0.40") {
		t.Errorf("cost missing without a ledger:\n%s", out)
	}
}
