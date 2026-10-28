package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseWorktrees(t *testing.T) {
	in := "worktree /src/app\x00HEAD abc1234def\x00branch refs/heads/main\x00\x00" +
		"worktree /src/app-a\x00HEAD 1111111aaa\x00branch refs/heads/feature/a\x00locked busy\x00\x00" +
		"worktree /src/app-gone\x00HEAD 2222222bbb\x00detached\x00prunable gitdir file points to non-existent location\x00\x00"
	got := parseWorktrees([]byte(in))
	if len(got) != 3 {
		t.Fatalf("got %d worktrees, want 3", len(got))
	}
	if !got[0].Main || got[0].Branch != "main" {
		t.Errorf("main worktree parsed wrong: %+v", got[0])
	}
	if got[1].Branch != "feature/a" || !got[1].Locked || got[1].LockReason != "busy" {
		t.Errorf("feature worktree parsed wrong: %+v", got[1])
	}
	if !got[2].Detached || !got[2].Prunable || got[2].Name() != "detached @ 2222222" {
		t.Errorf("prunable worktree parsed wrong: %+v", got[2])
	}
}

func TestParseNumstatAndNameStatus(t *testing.T) {
	num := "3\t1\tsrc/a.go\x00-\t-\tlogo.png\x000\t0\t\x00old name.txt\x00new name.txt\x00"
	counts := parseNumstat([]byte(num))
	if c := counts["src/a.go"]; c.added != 3 || c.deleted != 1 {
		t.Errorf("a.go counts = %+v", c)
	}
	if !counts["logo.png"].binary {
		t.Error("logo.png should be binary")
	}
	if _, ok := counts["new name.txt"]; !ok {
		t.Error("rename should be keyed by its new path")
	}

	names := parseNameStatus([]byte("M\x00src/a.go\x00R100\x00old name.txt\x00new name.txt\x00D\x00gone.txt\x00"))
	if len(names) != 3 {
		t.Fatalf("got %d entries, want 3", len(names))
	}
	if names[1].Status != Renamed || names[1].OldPath != "old name.txt" || names[1].Path != "new name.txt" {
		t.Errorf("rename parsed wrong: %+v", names[1])
	}
	if names[2].Status != Deleted || names[2].Path != "gone.txt" {
		t.Errorf("delete parsed wrong: %+v", names[2])
	}
}

func TestParseStatus(t *testing.T) {
	in := "1 .M N... 100644 100644 100644 aaa bbb src/with space.go\x00" +
		"2 R. N... 100644 100644 100644 aaa bbb R100 new.go\x00old.go\x00" +
		"u UU N... 100644 100644 100644 100644 aaa bbb ccc conflict.go\x00" +
		"? notes/todo.md\x00"
	got := parseStatus([]byte(in))
	want := []statusEntry{{'1', "src/with space.go"}, {'2', "new.go"}, {'u', "conflict.go"}, {'?', "notes/todo.md"}}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// testRepo builds a repository with a main branch and two feature worktrees
// that both touch shared.txt.
func testRepo(t *testing.T) (*Repo, string) {
	t.Helper()
	if Available() != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	main := filepath.Join(root, "app")
	must(t, os.MkdirAll(main, 0o755))
	g := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(path, content string) {
		t.Helper()
		must(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must(t, os.WriteFile(path, []byte(content), 0o644))
	}

	g(main, "init", "-q", "-b", "main")
	write(filepath.Join(main, "shared.txt"), "one\ntwo\nthree\n")
	write(filepath.Join(main, "keep.txt"), "keep\n")
	g(main, "add", ".")
	g(main, "commit", "-q", "-m", "initial")

	a := filepath.Join(root, "app-a")
	b := filepath.Join(root, "app-b")
	g(main, "worktree", "add", "-q", a, "-b", "feature/a")
	g(main, "worktree", "add", "-q", b, "-b", "feature/b")

	// feature/a: a committed edit, plus an untracked file.
	write(filepath.Join(a, "shared.txt"), "one\nTWO\nthree\nfour\n")
	g(a, "commit", "-q", "-am", "edit shared")
	write(filepath.Join(a, "src", "new file.go"), "package x\n\nfunc X() {}\n")

	// feature/b: an uncommitted edit of the same file and a rename.
	write(filepath.Join(b, "shared.txt"), "one\ntwo\n")
	g(b, "mv", "keep.txt", "kept.txt")

	r, err := Open(context.Background(), main)
	if err != nil {
		t.Fatal(err)
	}
	return r, root
}

func TestScanBranchMode(t *testing.T) {
	r, _ := testRepo(t)
	ctx := context.Background()

	base, err := DetectBase(ctx, r, "")
	if err != nil || base != "main" {
		t.Fatalf("DetectBase = %q, %v; want main", base, err)
	}
	snap, err := Scan(ctx, r, base, ModeBranch)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.States) != 3 {
		t.Fatalf("got %d worktrees, want 3", len(snap.States))
	}
	if r.Name != "app" {
		t.Errorf("repo name = %q, want app", r.Name)
	}

	byBranch := map[string]State{}
	for _, st := range snap.States {
		if st.Err != nil {
			t.Fatalf("%s: %v", st.Name(), st.Err)
		}
		byBranch[st.Branch] = st
	}

	if n := len(byBranch["main"].Files); n != 0 {
		t.Errorf("main should be clean, has %d files", n)
	}

	a := byBranch["feature/a"]
	if a.Ahead != 1 || a.Behind != 0 {
		t.Errorf("feature/a ahead/behind = %d/%d, want 1/0", a.Ahead, a.Behind)
	}
	shared := find(t, a.Files, "shared.txt")
	if shared.Status != Modified || shared.Added != 2 || shared.Deleted != 1 || shared.Uncommitted {
		t.Errorf("feature/a shared.txt = %+v", shared)
	}
	nf := find(t, a.Files, "src/new file.go")
	if nf.Status != Untracked || nf.Added != 3 {
		t.Errorf("untracked file = %+v", nf)
	}

	b := byBranch["feature/b"]
	if f := find(t, b.Files, "shared.txt"); !f.Uncommitted || f.Deleted != 1 {
		t.Errorf("feature/b shared.txt = %+v", f)
	}
	if f := find(t, b.Files, "kept.txt"); f.Status != Renamed || f.OldPath != "keep.txt" {
		t.Errorf("rename = %+v", f)
	}

	if got := snap.Overlaps["shared.txt"]; len(got) != 2 {
		t.Errorf("shared.txt overlap = %v, want two worktrees", got)
	}
	if snap.OverlapCount() != 1 {
		t.Errorf("overlap count = %d, want 1", snap.OverlapCount())
	}

	// Diffs line up with the file list.
	d, err := Diff(ctx, a, shared)
	if err != nil || !strings.Contains(d, "+TWO") {
		t.Errorf("diff of shared.txt = %q, %v", d, err)
	}
	d, err = Diff(ctx, a, nf)
	if err != nil || !strings.Contains(d, "+func X() {}") {
		t.Errorf("diff of untracked file = %q, %v", d, err)
	}
}

func TestScanUncommittedMode(t *testing.T) {
	r, _ := testRepo(t)
	snap, err := Scan(context.Background(), r, "main", ModeUncommitted)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range snap.States {
		if st.Branch != "feature/a" {
			continue
		}
		// The committed edit to shared.txt no longer counts; only the
		// untracked file is left.
		if len(st.Files) != 1 || st.Files[0].Path != "src/new file.go" {
			t.Errorf("feature/a files = %+v", st.Files)
		}
	}
	if snap.OverlapCount() != 0 {
		t.Errorf("overlaps = %v, want none", snap.Overlaps)
	}
}

func TestOpenAcceptsGitDirs(t *testing.T) {
	_, root := testRepo(t)
	ctx := context.Background()
	main := filepath.Join(root, "app")
	a := filepath.Join(root, "app-a")
	cases := map[string]string{
		main:                                main,
		main + string(filepath.Separator):   main,
		filepath.Join(main, ".git"):         main,
		filepath.Join(main, ".git", "refs"): main,
		filepath.Join(main, ".git", "worktrees", "app-a"): a,
		filepath.Join(a, "src"):                           a,
		filepath.Join(a, "src", "new file.go"):            a,
		filepath.Join(main, "shared.txt"):                 main,
	}
	for in, want := range cases {
		r, err := Open(ctx, in)
		if err != nil {
			t.Errorf("Open(%s): %v", in, err)
			continue
		}
		if !SamePath(r.Here, resolve(t, want)) {
			t.Errorf("Open(%s).Here = %s, want %s", in, r.Here, want)
		}
		if r.Name != "app" {
			t.Errorf("Open(%s).Name = %q, want app", in, r.Name)
		}
	}
	if _, err := Open(ctx, filepath.Join(root, "missing")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing path error = %v", err)
	}
}

func TestRemoveWorktree(t *testing.T) {
	r, root := testRepo(t)
	ctx := context.Background()
	list, err := Worktrees(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	byBranch := map[string]Worktree{}
	for _, w := range list {
		byBranch[w.Branch] = w
	}

	if _, err := RemoveWorktree(ctx, r, byBranch["main"], RemoveOptions{Force: true}); err == nil {
		t.Error("removing the main worktree should fail")
	}

	// feature/b has uncommitted edits, so git must refuse without Force.
	b := byBranch["feature/b"]
	if _, err := RemoveWorktree(ctx, r, b, RemoveOptions{}); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty remove error = %v", err)
	}
	if _, err := os.Stat(b.Path); err != nil {
		t.Fatalf("refused removal still touched the folder: %v", err)
	}

	res, err := RemoveWorktree(ctx, r, b, RemoveOptions{Force: true, DeleteBranch: true})
	if err != nil || res.FolderLeft || res.BranchErr != nil {
		t.Fatalf("forced remove = %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(root, "app-b")); !os.IsNotExist(err) {
		t.Errorf("folder still exists: %v", err)
	}
	if _, err := run(ctx, r.CommonDir, "rev-parse", "--verify", "refs/heads/feature/b"); err == nil {
		t.Error("branch feature/b should be deleted")
	}

	// Removing a worktree whose folder is already gone just tidies up.
	a := byBranch["feature/a"]
	must(t, os.RemoveAll(a.Path))
	if _, err := RemoveWorktree(ctx, r, a, RemoveOptions{}); err != nil {
		t.Fatalf("removing a missing worktree: %v", err)
	}
	if _, err := run(ctx, r.CommonDir, "rev-parse", "--verify", "refs/heads/feature/a"); err != nil {
		t.Error("branch feature/a should be kept")
	}
	if list, _ := Worktrees(ctx, r); len(list) != 1 {
		t.Errorf("%d worktrees left, want 1", len(list))
	}
}

// resolve expands short (8.3) and symlinked temp paths the way git reports
// them.
func resolve(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOpenNotRepo(t *testing.T) {
	if Available() != nil {
		t.Skip("git not installed")
	}
	_, err := Open(context.Background(), t.TempDir())
	if err != ErrNotRepo {
		t.Fatalf("err = %v, want ErrNotRepo", err)
	}
}

func find(t *testing.T, files []FileChange, path string) FileChange {
	t.Helper()
	for _, f := range files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("%s not in %+v", path, files)
	return FileChange{}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
