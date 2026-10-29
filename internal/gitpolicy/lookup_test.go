package gitpolicy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/git"
)

// testRepo makes main (on main), a feature/a worktree, and an origin that
// has feature/pushed.
func testRepo(t *testing.T) (main, feature string) {
	t.Helper()
	if git.Available() != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	main = filepath.Join(root, "app")
	feature = filepath.Join(root, "app-a")
	origin := filepath.Join(root, "origin.git")
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
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	g(root, "init", "-q", "--bare", origin)
	g(main, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(main, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(main, "add", ".")
	g(main, "commit", "-q", "-m", "initial")
	g(main, "config", "alias.st", "status")
	g(main, "remote", "add", "origin", origin)
	g(main, "branch", "feature/pushed")
	g(main, "push", "-q", "origin", "feature/pushed")
	g(main, "worktree", "add", "-q", feature, "-b", "feature/a")
	return main, feature
}

func TestGitLookup(t *testing.T) {
	main, feature := testRepo(t)
	lk := GitLookup(context.Background(), feature)

	if b, err := lk.CurrentBranch(main); err != nil || b != "main" {
		t.Errorf("CurrentBranch(main) = %q, %v", b, err)
	}
	if b, err := lk.CurrentBranch(feature); err != nil || b != "feature/a" {
		t.Errorf("CurrentBranch(feature) = %q, %v", b, err)
	}
	if _, err := lk.CurrentBranch(t.TempDir()); err == nil {
		t.Error("CurrentBranch outside a repository should fail")
	}
	if v, ok := lk.Alias("st"); !ok || v != "status" {
		t.Errorf("Alias(st) = %q, %v", v, ok)
	}
	if _, ok := lk.Alias("nope"); ok {
		t.Error("Alias(nope) should not exist")
	}
	if !lk.RefExists("refs/heads/feature/a") || !lk.RefExists("main") || lk.RefExists("refs/heads/nope") {
		t.Error("RefExists is wrong")
	}
	if !lk.Pushed("feature/pushed") || lk.Pushed("feature/a") {
		t.Error("Pushed is wrong")
	}

	sc := Scope{Worktree: feature, Readable: []string{main}, Protected: []string{"main"}, Guarded: true}
	for cmd, want := range map[string]Verdict{
		"git commit -m x":             Allow,
		"git push origin main":        User,
		"git -C ../app commit -m x":   User,
		"git -C ../app log":           Allow,
		"git st":                      User,
		"git switch main":             User,
		"git rebase main feature/a":   Allow,
		"git rebase main":             Allow,
		"git checkout feature/pushed": Allow,
		"git checkout a.txt":          User,
		"go build ./...":              NotGit,
	} {
		d := Classify(cmd, Bash, sc, GitLookup(context.Background(), feature))
		if d.Verdict != want {
			t.Errorf("%q = %v (%s), want %v", cmd, d.Verdict, d.Reason, want)
		}
	}

	// HEAD resolves through the folder's real branch.
	d := Classify("git commit -m x", Bash, Scope{Worktree: main, Protected: []string{"main"}, Guarded: true}, GitLookup(context.Background(), main))
	if d.Verdict != User || !d.Protected {
		t.Errorf("commit on main = %+v", d)
	}
}
