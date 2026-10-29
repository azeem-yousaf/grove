package git

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCommitWorkLeavesExcludedFiles(t *testing.T) {
	main := mergeRepo(t)
	ctx := context.Background()
	dir := filepath.Join(filepath.Dir(main), "feature-c")
	if ok, err := CommitWork(ctx, dir, "nothing", nil); err != nil || ok {
		t.Fatalf("a clean worktree has nothing to commit: %v, %v", ok, err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\nmore\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1\n"), 0o644))
	if ok, err := CommitWork(ctx, dir, "Add more", []string{".env"}); err != nil || !ok {
		t.Fatalf("CommitWork = %v, %v", ok, err)
	}
	out, err := run(ctx, dir, "show", "--name-only", "--format=%s", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(out)); !slices.Equal(got, []string{"Add", "more", "c.txt", "new.txt"}) {
		t.Errorf("committed %q", got)
	}
	if st, _ := run(ctx, dir, "status", "--porcelain"); strings.TrimSpace(string(st)) != "?? .env" {
		t.Errorf("status after = %q, want only .env left", st)
	}
}

func TestConcludeMerge(t *testing.T) {
	main := mergeRepo(t)
	ctx := context.Background()
	if left, err := ConcludeMerge(ctx, main, nil); err != nil || left != nil {
		t.Fatalf("without a merge: %v, %v", left, err)
	}
	if _, err := Merge(ctx, main, "feature/a"); err != nil {
		t.Fatal(err)
	}
	if paths, err := Merge(ctx, main, "feature/b"); err != nil || len(paths) != 1 {
		t.Fatalf("want a conflict: %v, %v", paths, err)
	}
	left, err := ConcludeMerge(ctx, main, nil)
	if err != nil || !slices.Equal(left, []string{"shared.txt"}) {
		t.Fatalf("markers left = %v, %v", left, err)
	}
	must(t, os.WriteFile(filepath.Join(main, "shared.txt"), []byte("one\nTWO from a and b\nthree\n"), 0o644))
	if left, err := ConcludeMerge(ctx, main, nil); err != nil || left != nil {
		t.Fatalf("resolved: %v, %v", left, err)
	}
	if busy, _ := MergeInProgress(ctx, main); busy {
		t.Error("the merge should be committed")
	}
	if n, _ := CommitsBetween(ctx, main, "main", "feature/b"); n != 0 {
		t.Error("feature/b should be merged")
	}
}
