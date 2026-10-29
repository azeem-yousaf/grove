package ui

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// groupedFake puts fakeModel's feature/auth and feature/cart in one group
// of a running plan.
func groupedFake(t *testing.T, w, h int) *Model {
	t.Helper()
	m := fakeModel(t, w, h)
	m.plans = []*plan.Plan{{ID: "p", Title: "Shop", Status: plan.Running,
		Groups: []plan.Group{{ID: "g", Name: "Checkout", Icon: "🛒"}},
		Tasks: []plan.Task{
			{ID: "a", Title: "Sign in", Branch: "feature/auth", Path: filepath.FromSlash("/src/shop-auth"), Group: "g", Status: plan.Working},
			{ID: "c", Title: "Cart", Branch: "feature/cart", Path: filepath.FromSlash("/src/shop-cart"), Group: "g", Status: plan.Finished},
		}}}
	m.rebuildRows()
	return m
}

func TestGroupFramesItsWorktrees(t *testing.T) {
	m := groupedFake(t, 160, 40)
	var kinds []rowKind
	for _, r := range m.rows {
		if r.kind != rowFile {
			kinds = append(kinds, r.kind)
		}
	}
	if want := []rowKind{rowTree, rowGroup, rowTree, rowTree}; !slices.Equal(kinds, want) {
		t.Fatalf("row kinds = %v, want %v", kinds, want)
	}
	r, _ := m.selected()
	if m.snap.States[r.wt].Branch != "feature/auth" || r.grp != 0 {
		t.Fatalf("selection = %+v, want feature/auth inside the group", r)
	}
	out := plain(m)
	for _, want := range []string{"╭─ ▾ 🛒 Checkout", "2 worktrees", "1/2 done", "│ ▾ ● feature/auth  ◆ here", "╰──"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "│  ▾ ● main") {
		t.Error("main is drawn inside the group")
	}
}

func TestGroupFoldsAndKeepsSelection(t *testing.T) {
	m := groupedFake(t, 160, 40)
	m.moveTo(m.groupRow(0))
	m.refreshDiffView()
	if !m.groupSelected() {
		t.Fatal("cursor isn't on the group header")
	}
	out := plain(m)
	for _, want := range []string{"Members", "feature/cart", "Merging", "Group chat"} {
		if !strings.Contains(out, want) {
			t.Errorf("group summary lacks %q:\n%s", want, out)
		}
	}
	press(m, "enter")
	if len(m.rows) != 2 || !m.groupSelected() {
		t.Fatalf("folded group: %d rows, group selected %v", len(m.rows), m.groupSelected())
	}
	if out := plain(m); strings.Contains(out, "╭─ ▾") || !strings.Contains(out, "▸ 🛒 Checkout") {
		t.Errorf("folded group should be one line:\n%s", out)
	}
	// A rescan keeps the group selected and folded.
	m.Update(scanMsg{seq: m.scanSeq, snap: m.snap})
	if !m.groupSelected() || len(m.rows) != 2 {
		t.Errorf("after a rescan: group selected %v, %d rows", m.groupSelected(), len(m.rows))
	}
	press(m, "right")
	if len(m.rows) == 2 {
		t.Error("right didn't unfold the group")
	}
	// Left on a folded worktree goes up to its group.
	m.moveTo(m.groupRow(0) + 1)
	press(m, "left", "left")
	if !m.groupSelected() {
		t.Error("left on a folded member should select its group")
	}
}

func TestGroupHeaderOpensChat(t *testing.T) {
	m := groupedFake(t, 160, 40)
	m.store = nil
	m.moveTo(m.groupRow(0))
	press(m, "t")
	if m.focus == focusGroup {
		t.Error("no store, so no chat to open")
	}
}

func TestNarrowListUsesARail(t *testing.T) {
	m := groupedFake(t, 100, 30)
	out := plain(m)
	if strings.Contains(out, "╭─ ▾") || !strings.Contains(out, "┃ ▾ ● feature/auth  ◆ here") {
		t.Errorf("a narrow list should mark the group with a bar:\n%s", out)
	}
}

func TestGroupFilteredAway(t *testing.T) {
	m := groupedFake(t, 160, 40)
	m.filterQuery = "nothing-matches-this"
	m.rebuildRows()
	if len(m.rows) != 0 {
		t.Errorf("rows = %+v, want none", m.rows)
	}
	m.filterQuery = "login"
	m.rebuildRows()
	if len(m.rows) != 3 || m.rows[0].kind != rowGroup {
		t.Errorf("rows = %+v, want the group, feature/auth and login.ts", m.rows)
	}
}

func TestGroupConflictsColourTheBorder(t *testing.T) {
	m := groupedFake(t, 160, 40)
	predicted(m, []git.Prediction{{A: 1, B: 2, Files: []git.FileConflict{{Path: "src/api/client.ts"}}}})
	o := m.groupStats(0)
	if o.conflicts != 1 || m.groupColor(0, o) != m.st.p.conflict {
		t.Errorf("conflicts = %d, colour %v", o.conflicts, m.groupColor(0, o))
	}
	if out := plain(m); !strings.Contains(out, "✗ 1 conflict") {
		t.Errorf("header lacks the conflict count:\n%s", out)
	}
}

func TestSplitGroupIcon(t *testing.T) {
	for _, c := range []struct{ in, icon, name string }{
		{"🛒 Checkout", "🛒", "Checkout"},
		{"  🛒Checkout ", "🛒", "Checkout"},
		{"Checkout", "", "Checkout"},
		{"🛒", "", "🛒"},
		{"❤️ Love", "", "❤️ Love"}, // text-width heart: not a usable icon
		{"⇄ Sync", "", "⇄ Sync"},
		{"", "", ""},
	} {
		icon, name := splitGroupIcon(c.in)
		if icon != c.icon || name != c.name {
			t.Errorf("splitGroupIcon(%q) = %q, %q; want %q, %q", c.in, icon, name, c.icon, c.name)
		}
	}
	if groupIcon(plan.Group{Icon: "👨‍👩‍👧"}) != "⇄" {
		t.Error("a joined emoji should fall back to ⇄")
	}
}
