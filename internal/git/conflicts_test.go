package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// mergeRepo builds main plus feature/a and feature/b, which edit the same
// line of shared.txt in different ways, and feature/c, which only adds a
// file. Merging a and b each look clean on their own; together they
// conflict.
func mergeRepo(t *testing.T) (main string) {
	t.Helper()
	if Available() != nil {
		t.Skip("git not installed")
	}
	// Merge and commit-tree need an identity, and the user's config must
	// not change the results.
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	root := t.TempDir()
	main = filepath.Join(root, "app")
	must(t, os.MkdirAll(main, 0o755))
	g := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(path, content string) {
		t.Helper()
		must(t, os.WriteFile(path, []byte(content), 0o644))
	}
	g(main, "init", "-q", "-b", "main")
	write(filepath.Join(main, "shared.txt"), "one\ntwo\nthree\n")
	g(main, "add", ".")
	g(main, "commit", "-q", "-m", "initial")
	for _, b := range []struct{ name, file, content string }{
		{"feature/a", "shared.txt", "one\nTWO from a\nthree\n"},
		{"feature/b", "shared.txt", "one\ntwo from b\nthree\n"},
		{"feature/c", "c.txt", "c\n"},
	} {
		dir := filepath.Join(root, strings.ReplaceAll(b.name, "/", "-"))
		g(main, "worktree", "add", "-q", dir, "-b", b.name)
		write(filepath.Join(dir, b.file), b.content)
		g(dir, "add", ".")
		g(dir, "commit", "-q", "-m", b.name)
	}
	return main
}

func TestMergeConflicts(t *testing.T) {
	main := mergeRepo(t)
	ctx := context.Background()

	tree, paths, err := MergeConflicts(ctx, main, "main", "feature/a")
	if err != nil || len(paths) != 0 || tree == "" {
		t.Fatalf("main <- feature/a = %q, %v, %v; want a clean tree", tree, paths, err)
	}
	_, paths, err = MergeConflicts(ctx, main, "feature/a", "feature/b")
	if err != nil || !slices.Equal(paths, []string{"shared.txt"}) {
		t.Fatalf("feature/a <- feature/b = %v, %v; want [shared.txt] only, without git's messages", paths, err)
	}
}

func TestPreviewMergesInOrder(t *testing.T) {
	main := mergeRepo(t)
	ctx := context.Background()

	steps := PreviewMerges(ctx, main, "main", []string{"feature/c", "feature/a", "feature/b"})
	if len(steps) != 3 {
		t.Fatalf("got %d steps, want 3: %+v", len(steps), steps)
	}
	if steps[0].Err != nil || len(steps[0].Conflicts) != 0 || len(steps[1].Conflicts) != 0 {
		t.Errorf("c then a should merge cleanly: %+v", steps[:2])
	}
	// b is clean against main alone, but not after a.
	if !slices.Equal(steps[2].Conflicts, []string{"shared.txt"}) {
		t.Errorf("b after a = %+v, want a conflict in shared.txt", steps[2])
	}
	if out, _ := exec.Command("git", "-C", main, "status", "--porcelain").Output(); len(out) != 0 {
		t.Errorf("preview changed the worktree:\n%s", out)
	}

	// Steps after a conflict are not previewed.
	steps = PreviewMerges(ctx, main, "feature/a", []string{"feature/b", "feature/c"})
	if len(steps) != 1 || len(steps[0].Conflicts) != 1 {
		t.Errorf("preview should stop at the first conflict, got %+v", steps)
	}
}

func TestMergeStopsOnConflict(t *testing.T) {
	main := mergeRepo(t)
	ctx := context.Background()

	if n, err := CommitsBetween(ctx, main, "main", "feature/a"); err != nil || n != 1 {
		t.Fatalf("CommitsBetween = %d, %v; want 1", n, err)
	}
	if paths, err := Merge(ctx, main, "feature/a"); err != nil || len(paths) != 0 {
		t.Fatalf("merging feature/a = %v, %v; want clean", paths, err)
	}
	if busy, _ := MergeInProgress(ctx, main); busy {
		t.Error("a clean merge should not leave a merge in progress")
	}
	paths, err := Merge(ctx, main, "feature/b")
	if err != nil || !slices.Equal(paths, []string{"shared.txt"}) {
		t.Fatalf("merging feature/b = %v, %v; want a conflict in shared.txt", paths, err)
	}
	if busy, err := MergeInProgress(ctx, main); err != nil || !busy {
		t.Fatalf("MergeInProgress = %v, %v; want true", busy, err)
	}
	if dirty, _ := HasTrackedChanges(ctx, main); !dirty {
		t.Error("a conflicted merge should count as tracked changes")
	}
	must(t, AbortMerge(ctx, main))
	if busy, _ := MergeInProgress(ctx, main); busy {
		t.Error("abort should end the merge")
	}
	if dirty, _ := HasTrackedChanges(ctx, main); dirty {
		t.Error("abort should leave the worktree clean")
	}

	// A failure that isn't a conflict is an error.
	if _, err := Merge(ctx, main, "no-such-branch"); err == nil {
		t.Error("merging a missing branch should fail")
	}
}
