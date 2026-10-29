package ui

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/git"
)

// clientConflict is fakeModel's overlap, src/api/client.ts in feature/auth
// (1) and feature/cart (2), predicted to conflict.
func clientConflict() []git.Prediction {
	return []git.Prediction{{A: 1, B: 2, Files: []git.FileConflict{{
		Path: "src/api/client.ts",
		A:    []git.LineRange{{Start: 10, End: 12}},
		B:    []git.LineRange{{Start: 3, End: 3}},
	}}}}
}

// predicted delivers predictions for the model's current snapshot the way
// a finished prediction run would.
func predicted(m *Model, preds []git.Prediction) {
	m.predictSeq++
	m.predicting = true
	m.Update(predictMsg{seq: m.predictSeq, snap: m.snap, preds: preds})
}

// linesWith returns the screen lines containing s.
func linesWith(out, s string) []string {
	var found []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, s) {
			found = append(found, l)
		}
	}
	return found
}

func TestConflictMarksAndHeader(t *testing.T) {
	m := fakeModel(t, 160, 40)
	out := plain(m)
	if !strings.Contains(out, "⚠ 1 overlap") || strings.Contains(out, "✗") {
		t.Fatalf("before a prediction the overlap is a plain ⚠:\n%s", out)
	}

	// A prediction that merges cleanly keeps the ⚠.
	predicted(m, []git.Prediction{{A: 1, B: 2}})
	if out := plain(m); !strings.Contains(out, "⚠ 1 overlap") || strings.Contains(out, "✗") {
		t.Errorf("a clean merge should stay ⚠:\n%s", out)
	}

	predicted(m, clientConflict())
	out = plain(m)
	if !strings.Contains(out, "✗ 1 conflict") || strings.Contains(out, "⚠ 1 overlap") {
		t.Errorf("header should count the conflict instead of the overlap:\n%s", out)
	}
	rows := linesWith(out, "M src/api/client.ts")
	if len(rows) != 2 {
		t.Fatalf("want client.ts in both worktrees, got %q", rows)
	}
	for _, l := range rows {
		if !strings.Contains(l, "✗") || strings.Contains(l, "⚠") {
			t.Errorf("client.ts should be marked ✗: %q", l)
		}
	}
	if got := m.conflictsFor(1, "src/api/client.ts"); !slices.Equal(got, []int{2}) {
		t.Errorf("conflictsFor(auth) = %v, want [2]", got)
	}
	if got := m.conflictsFor(2, "src/api/client.ts"); !slices.Equal(got, []int{1}) {
		t.Errorf("conflictsFor(cart) = %v, want [1]", got)
	}
	if m.willConflict(1, "src/auth/login.ts") {
		t.Error("login.ts is in one worktree only")
	}

	// A failed prediction says nothing: back to ⚠.
	predicted(m, []git.Prediction{{A: 1, B: 2, Err: context.DeadlineExceeded}})
	if out := plain(m); !strings.Contains(out, "⚠ 1 overlap") || strings.Contains(out, "✗") {
		t.Errorf("an unknown result should stay ⚠:\n%s", out)
	}
}

func TestStalePredictionsAreDropped(t *testing.T) {
	m := fakeModel(t, 160, 40)
	m.predictSeq = 2
	m.Update(predictMsg{seq: 1, snap: m.snap, preds: clientConflict()})
	if len(m.predictions) != 0 {
		t.Error("a prediction from an older run was applied")
	}
	m.Update(predictMsg{seq: 2, snap: &git.Snapshot{}, preds: clientConflict()})
	if len(m.predictions) != 0 {
		t.Error("a prediction for an older snapshot was applied")
	}
}

func TestPredictionsFollowWorktreesAcrossScans(t *testing.T) {
	m := fakeModel(t, 160, 40)
	predicted(m, clientConflict())
	// The next scan lists the worktrees in another order.
	snap := *m.snap
	snap.States = []git.State{m.snap.States[2], m.snap.States[0], m.snap.States[1]}
	snap.Overlaps = map[string][]int{"src/api/client.ts": {0, 2}}
	m.Update(scanMsg{seq: m.scanSeq, snap: &snap})
	if got := m.conflictsFor(2, "src/api/client.ts"); !slices.Equal(got, []int{0}) {
		t.Fatalf("auth is now 2 and cart 0; conflictsFor(2) = %v", got)
	}
	if got := m.conflictLines(2, "src/api/client.ts"); !slices.Equal(got, []git.LineRange{{Start: 10, End: 12}}) {
		t.Errorf("auth's lines should follow it: %v", got)
	}
	if got := m.conflictLines(0, "src/api/client.ts"); !slices.Equal(got, []git.LineRange{{Start: 3, End: 3}}) {
		t.Errorf("cart's lines should follow it: %v", got)
	}

	// A worktree that disappears takes its predictions with it.
	snap2 := snap
	snap2.States = snap.States[1:]
	snap2.Overlaps = nil
	m.Update(scanMsg{seq: m.scanSeq, snap: &snap2})
	if len(m.predictions) != 0 {
		t.Errorf("predictions for a removed worktree remain: %+v", m.predictions)
	}
}

func TestPredictionWaitsForRemoval(t *testing.T) {
	m := fakeModel(t, 160, 40)
	m.repo.CommonDir = t.TempDir()
	m.predicting = false
	m.removing = &removal{}
	if m.startPredict() != nil {
		t.Error("prediction started while a worktree is being removed")
	}
	m.removing = nil
	if m.startPredict() == nil {
		t.Error("prediction should start once nothing is being removed")
	}
	if m.startPredict() != nil || !m.repredict {
		t.Error("a second request while one runs should be queued")
	}
}

func TestOverlapsKeyCycles(t *testing.T) {
	m := fakeModel(t, 160, 40)
	// Without conflicts: off → overlaps → off.
	press(m, "o")
	if m.overlapView != overlapsShown || m.fileRowCount() != 2 {
		t.Fatalf("o should show the 2 overlapping files, view %d with %d", m.overlapView, m.fileRowCount())
	}
	press(m, "o")
	if m.overlapView != overlapsOff || m.fileRowCount() != 6 {
		t.Fatalf("with nothing conflicting, o should go back to every file, view %d", m.overlapView)
	}

	// Another overlap that merges cleanly, so the conflicts view differs.
	snap := *m.snap
	snap.States = slices.Clone(m.snap.States)
	st := snap.States[2]
	st.Files = append(slices.Clone(st.Files), git.FileChange{Path: "src/auth/login.ts", Status: git.Modified, Added: 1})
	snap.States[2] = st
	snap.Overlaps = map[string][]int{"src/api/client.ts": {1, 2}, "src/auth/login.ts": {1, 2}}
	m.Update(scanMsg{seq: m.scanSeq, snap: &snap})
	predicted(m, clientConflict())

	press(m, "o")
	if m.overlapView != overlapsShown || m.fileRowCount() != 4 {
		t.Fatalf("overlaps view shows %d files, want 4", m.fileRowCount())
	}
	press(m, "o")
	if m.overlapView != conflictsShown || m.fileRowCount() != 2 {
		t.Fatalf("conflicts view shows %d files, want 2", m.fileRowCount())
	}
	if out := plain(m); !strings.Contains(out, "conflicts only") {
		t.Errorf("footer should say the list shows conflicts only:\n%s", out)
	}
	for _, r := range m.rows {
		if f := m.snap.States[r.wt].Files[r.file]; r.kind == rowFile && f.Path != "src/api/client.ts" {
			t.Errorf("conflicts view lists %s", f.Path)
		}
	}
	press(m, "o")
	if m.overlapView != overlapsOff || m.fileRowCount() != 7 {
		t.Fatalf("o should cycle back to every file, view %d with %d", m.overlapView, m.fileRowCount())
	}
	press(m, "o", "o", "esc")
	if m.overlapView != overlapsOff {
		t.Error("esc should clear the conflicts view")
	}
}

const conflictDiff = `diff --git a/src/api/client.ts b/src/api/client.ts
--- a/src/api/client.ts
+++ b/src/api/client.ts
@@ -8,5 +8,6 @@ export class Client {
 a
 b
-c
+c2
+c3
 d
 e
@@ -40,3 +41,3 @@ function other() {
 x
-y
+y2
 z
`

func TestConflictBannerAndHunks(t *testing.T) {
	m := fakeModel(t, 160, 40)
	press(m, "down") // src/api/client.ts in feature/auth
	m.Update(diffMsg{key: m.diffWanted, raw: conflictDiff})
	out := plain(m)
	if !strings.Contains(out, "Also changed in feature/cart") || strings.Contains(out, "✗ conflicts") {
		t.Fatalf("before a prediction the banner is the overlap one:\n%s", out)
	}

	predicted(m, clientConflict())
	out = plain(m)
	if !strings.Contains(out, "✗ Conflicts with feature/cart") {
		t.Errorf("banner should name the conflicting worktree:\n%s", out)
	}
	if !strings.Contains(out, "✗ conflicts ⋯ export class Client") {
		t.Errorf("the hunk over lines 10-12 should be marked:\n%s", out)
	}
	if strings.Contains(out, "✗ conflicts ⋯ function other") || !strings.Contains(out, "⋯ function other") {
		t.Errorf("the hunk at line 41 doesn't conflict:\n%s", out)
	}
	if n := strings.Count(out, "┃"); n != 7 {
		t.Errorf("the conflicting hunk's 7 lines should have a bar, found %d", n)
	}
}

func TestClashingHunks(t *testing.T) {
	pd := parseDiff(conflictDiff)
	kinds := func(marked []bool) (hunks []bool) {
		for i, l := range pd.lines {
			if l.kind == lineHunk {
				hunks = append(hunks, marked[i])
			}
		}
		return hunks
	}
	for _, c := range []struct {
		name  string
		clash []git.LineRange
		want  []bool
	}{
		{"none", nil, []bool{false, false}},
		{"first", []git.LineRange{{Start: 11, End: 11}}, []bool{true, false}},
		{"second", []git.LineRange{{Start: 42, End: 42}}, []bool{false, true}},
		// Context lines alone don't make a hunk conflict.
		{"context only", []git.LineRange{{Start: 13, End: 13}}, []bool{false, false}},
		// The removed y sits after line 41.
		{"deletion", []git.LineRange{{Start: 41, End: 41}}, []bool{false, true}},
	} {
		if got := kinds(clashingHunks(pd.lines, c.clash)); !slices.Equal(got, c.want) {
			t.Errorf("%s: hunks marked %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOverviewShowsConflicts(t *testing.T) {
	m := fakeModel(t, 160, 40)
	predicted(m, clientConflict())
	press(m, "w")
	out := plain(m)
	for _, want := range []string{"✗ Conflicts", "feature/auth ⇄ feature/cart", "1 file: src/api/client.ts", "will conflict between feature/auth, feature/cart", "✗1"} {
		if !strings.Contains(out, want) {
			t.Errorf("overview is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "is changed in feature/auth") {
		t.Error("a conflicting file shouldn't also be listed as a plain overlap")
	}
}

func TestPredictAndMergeReviewUseTheCache(t *testing.T) {
	m, _ := mergeModel(t)
	m.predicting = false
	cmd := m.startPredict()
	if cmd == nil {
		t.Fatal("a scan of a repository with overlaps should start a prediction")
	}
	m.Update(cmd())
	if m.predictOff {
		t.Skip("git is older than 2.38")
	}
	a, b := -1, -1
	for i, s := range m.snap.States {
		switch s.Branch {
		case "feature/a":
			a = i
		case "feature/b":
			b = i
		}
	}
	if got := m.conflictsFor(a, "shared.txt"); !slices.Equal(got, []int{b}) {
		t.Fatalf("feature/a's shared.txt should conflict with feature/b, got %v", got)
	}
	if out := plain(m); !strings.Contains(out, "✗ 1 conflict") {
		t.Errorf("header should show the conflict:\n%s", out)
	}

	var mu sync.Mutex
	var asked []string
	defer func(f func(context.Context, string, string, string) (string, []string, error)) { mergeConflicts = f }(mergeConflicts)
	orig := mergeConflicts
	mergeConflicts = func(ctx context.Context, dir, target, source string) (string, []string, error) {
		mu.Lock()
		asked = append(asked, source)
		mu.Unlock()
		return orig(ctx, dir, target, source)
	}
	for i, r := range m.rows {
		if r.kind == rowTree && r.wt == a {
			m.cursor = i
		}
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	until(t, m, cmd, func() bool { return m.conflicts.stage != stageChecking })
	for _, ch := range m.conflicts.checks {
		if ch.branch == "feature/b" && !slices.Equal(ch.paths, []string{"shared.txt"}) {
			t.Errorf("feature/b into feature/a should conflict in shared.txt: %+v", ch)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(asked, "feature/b") {
		t.Errorf("feature/b was merged again although the prediction had it: %v", asked)
	}
	if !slices.Contains(asked, "feature/c") {
		t.Errorf("feature/c wasn't predicted, so git should be asked: %v", asked)
	}
}
