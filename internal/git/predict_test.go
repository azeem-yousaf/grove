package git

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseHunkAndRanges(t *testing.T) {
	for in, want := range map[string]hunk{
		"@@ -2 +2 @@":            {2, 1, 2, 1},
		"@@ -3,0 +4 @@ three":    {3, 0, 4, 1},
		"@@ -5,2 +4,0 @@":        {5, 2, 4, 0},
		"@@ -10,3 +12,4 @@ func": {10, 3, 12, 4},
	} {
		if got, ok := parseHunk(in); !ok || got != want {
			t.Errorf("parseHunk(%q) = %+v, %v; want %+v", in, got, ok, want)
		}
	}
	// A change of line 2 meets an insertion after line 2 and a change of
	// line 3, but not an insertion after line 3.
	change2 := hunk{2, 1, 2, 1}
	for h, want := range map[hunk]bool{
		{2, 0, 3, 1}: true,
		{3, 1, 3, 1}: true,
		{1, 0, 2, 1}: true,
		{3, 0, 4, 1}: false,
		{4, 2, 4, 2}: false,
	} {
		if got := touches(change2, h); got != want {
			t.Errorf("touches(%+v, %+v) = %v, want %v", change2, h, got, want)
		}
	}
	got := mergeRanges([]LineRange{{7, 9}, {1, 2}, {3, 3}, {8, 12}})
	if want := []LineRange{{1, 3}, {7, 12}}; !reflect.DeepEqual(got, want) {
		t.Errorf("mergeRanges = %v, want %v", got, want)
	}
	if ma, mi, ok := parseGitVersion("git version 2.39.3 (Apple Git-146)\n"); !ok || ma != 2 || mi != 39 {
		t.Errorf("parseGitVersion = %d.%d %v", ma, mi, ok)
	}
}

// predictRepo is testRepo with a helper to run git in its worktrees. It
// skips when git is older than 2.38.
func predictRepo(t *testing.T) (r *Repo, a, b string, g func(dir string, args ...string), write func(path, content string)) {
	t.Helper()
	r, root := testRepo(t)
	if ok, err := MergeTreeSupported(context.Background(), r.CommonDir); err != nil || !ok {
		t.Skip("git 2.38 or later needed")
	}
	g = func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write = func(path, content string) {
		t.Helper()
		must(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must(t, os.WriteFile(path, []byte(content), 0o644))
	}
	return r, filepath.Join(root, "app-a"), filepath.Join(root, "app-b"), g, write
}

// predictAB scans and predicts, returning the feature/a + feature/b pair
// with A as feature/a.
func predictAB(t *testing.T, p *Predictor, r *Repo) Prediction {
	t.Helper()
	ctx := context.Background()
	snap, err := Scan(ctx, r, "main", ModeBranch)
	if err != nil {
		t.Fatal(err)
	}
	var ia, ib int = -1, -1
	for i, st := range snap.States {
		switch st.Branch {
		case "feature/a":
			ia = i
		case "feature/b":
			ib = i
		}
	}
	for _, pr := range p.Predict(ctx, r, snap) {
		if pr.A > pr.B {
			t.Errorf("pair out of order: %d, %d", pr.A, pr.B)
		}
		if pr.A == min(ia, ib) && pr.B == max(ia, ib) {
			if pr.Err != nil {
				t.Fatal(pr.Err)
			}
			if ia > ib {
				for i, f := range pr.Files {
					pr.Files[i].A, pr.Files[i].B = f.B, f.A
				}
			}
			return pr
		}
	}
	t.Fatal("feature/a and feature/b were not checked")
	return Prediction{}
}

func TestPredictSameLines(t *testing.T) {
	r, a, b, g, write := predictRepo(t)
	ctx := context.Background()
	// a committed "one TWO three four"; b now commits its own line 2.
	must(t, os.RemoveAll(filepath.Join(a, "src")))
	write(filepath.Join(b, "shared.txt"), "one\ntwo from b\nthree\n")
	g(b, "commit", "-q", "-am", "b edits line 2")

	indexPath := filepath.Join(r.CommonDir, "worktrees", "app-a", "index")
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(indexPath)

	p := NewPredictor()
	pr := predictAB(t, p, r)
	want := []FileConflict{{Path: "shared.txt", A: []LineRange{{2, 2}}, B: []LineRange{{2, 2}}}}
	if !reflect.DeepEqual(pr.Files, want) {
		t.Errorf("files = %+v, want %+v", pr.Files, want)
	}
	if !pr.Clean {
		t.Error("both sides are committed, so the prediction should be clean")
	}

	// The merge review can reuse what Predict found.
	paths, ok := p.Cached(ctx, r.CommonDir, "feature/a", "feature/b")
	if !ok || !reflect.DeepEqual(paths, []string{"shared.txt"}) {
		t.Errorf("Cached = %v, %v", paths, ok)
	}
	if _, ok := p.Cached(ctx, r.CommonDir, "main", "feature/b"); ok {
		t.Error("main and feature/b were never predicted")
	}

	after, _ := os.ReadFile(indexPath)
	info2, _ := os.Stat(indexPath)
	if !bytes.Equal(before, after) || !info.ModTime().Equal(info2.ModTime()) {
		t.Error("the real index changed")
	}
}

func TestPredictSeparateParts(t *testing.T) {
	r, a, b, g, write := predictRepo(t)
	write(filepath.Join(a, "shared.txt"), "ONE\ntwo\nthree\n")
	g(a, "commit", "-q", "-am", "a edits line 1")
	write(filepath.Join(b, "shared.txt"), "one\ntwo\nTHREE\n")
	g(b, "commit", "-q", "-am", "b edits line 3")

	pr := predictAB(t, NewPredictor(), r)
	if len(pr.Files) != 0 {
		t.Errorf("separate edits should merge cleanly, got %+v", pr.Files)
	}
	if pr.Clean {
		t.Error("feature/a has an untracked file, so the prediction isn't clean")
	}
}

func TestPredictUncommittedOnly(t *testing.T) {
	r, a, b, g, write := predictRepo(t)
	write(filepath.Join(a, "shared.txt"), "ONE\ntwo\nthree\n")
	g(a, "commit", "-q", "-am", "a edits line 1")
	write(filepath.Join(b, "shared.txt"), "one\ntwo\nTHREE\n")
	g(b, "commit", "-q", "-am", "b edits line 3")

	p := NewPredictor()
	if pr := predictAB(t, p, r); len(pr.Files) != 0 {
		t.Fatalf("committed work should merge cleanly, got %+v", pr.Files)
	}

	// Uncommitted: a also edits line 3. Untracked: both add the same new
	// file with different content.
	indexPath := filepath.Join(r.CommonDir, "worktrees", "app-a", "index")
	before, _ := os.ReadFile(indexPath)
	info, _ := os.Stat(indexPath)
	write(filepath.Join(a, "shared.txt"), "ONE\ntwo\nthree from a\n")
	write(filepath.Join(b, "src", "new file.go"), "package y\n")

	pr := predictAB(t, p, r)
	want := []FileConflict{
		{Path: "shared.txt", A: []LineRange{{3, 3}}, B: []LineRange{{3, 3}}},
		{Path: "src/new file.go", A: []LineRange{{1, 3}}, B: []LineRange{{1, 1}}},
	}
	if !reflect.DeepEqual(pr.Files, want) {
		t.Errorf("files = %+v, want %+v", pr.Files, want)
	}
	after, _ := os.ReadFile(indexPath)
	info2, _ := os.Stat(indexPath)
	if !bytes.Equal(before, after) || !info.ModTime().Equal(info2.ModTime()) {
		t.Error("the real index changed")
	}

	// Nothing changed: the second call answers from the cache.
	runs := p.runs.Load()
	if pr := predictAB(t, p, r); !reflect.DeepEqual(pr.Files, want) {
		t.Errorf("cached files = %+v", pr.Files)
	}
	if p.runs.Load() != runs {
		t.Errorf("unchanged worktrees were merged again (%d runs, want %d)", p.runs.Load(), runs)
	}
	if len(p.cache) != 1 {
		t.Errorf("cache holds %d entries, want 1", len(p.cache))
	}
}
