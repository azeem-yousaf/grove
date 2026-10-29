package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in dir for a test, failing it on error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestUpstream(t *testing.T) {
	_, root := testRepo(t)
	ctx := context.Background()
	main, a := filepath.Join(root, "app"), filepath.Join(root, "app-a")

	// No remote at all.
	up, err := UpstreamOf(ctx, a)
	if err != nil || up != (Upstream{}) {
		t.Fatalf("no remote: %+v, %v", up, err)
	}

	bare := filepath.Join(root, "origin.git")
	gitIn(t, root, "init", "-q", "--bare", bare)
	gitIn(t, main, "remote", "add", "origin", bare)
	gitIn(t, main, "push", "-q", "-u", "origin", "main")

	// A remote but no upstream: the first push goes to origin.
	up, err = UpstreamOf(ctx, a)
	if err != nil || up.Name != "" || up.Remote != "origin" {
		t.Fatalf("before the first push: %+v, %v", up, err)
	}

	gitIn(t, a, "push", "-q", "-u", "origin", "feature/a")
	os.WriteFile(filepath.Join(a, "more.txt"), []byte("more\n"), 0o644)
	gitIn(t, a, "add", "more.txt")
	gitIn(t, a, "commit", "-q", "-m", "more")
	up, err = UpstreamOf(ctx, a)
	if err != nil || up.Name != "origin/feature/a" || up.Remote != "origin" || up.Ahead != 1 || up.Behind != 0 {
		t.Fatalf("after a commit: %+v, %v", up, err)
	}

	if remote, branch, ok := RemoteBranch(ctx, a, "origin/main"); !ok || remote != "origin" || branch != "main" {
		t.Errorf("RemoteBranch(origin/main) = %q %q %v", remote, branch, ok)
	}
	if _, _, ok := RemoteBranch(ctx, a, "main"); ok {
		t.Error("main is a local branch")
	}
}

func TestCommitArgs(t *testing.T) {
	_, root := testRepo(t)
	ctx := context.Background()
	b := filepath.Join(root, "app-b")
	// feature/b has shared.txt edited and keep.txt renamed to kept.txt
	// (staged). Add a file whose name is a glob pattern, and one to leave
	// out of the commit.
	os.WriteFile(filepath.Join(b, "[id].tsx"), []byte("page\n"), 0o644)
	os.WriteFile(filepath.Join(b, "id.tsx"), []byte("not me\n"), 0o644)

	// And delete a tracked file without telling git.
	gitIn(t, b, "add", "id.tsx")
	gitIn(t, b, "commit", "-q", "-m", "id", "--", "id.tsx")
	os.Remove(filepath.Join(b, "id.tsx"))
	os.WriteFile(filepath.Join(b, "other.txt"), []byte("not me\n"), 0o644)

	p, err := PlanCommit(b, []string{"[id].tsx", "shared.txt", "kept.txt", "keep.txt", "id.tsx"}, "Add the page\n\nAnd the rest.", false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cleanup()
	gitIn(t, b, p.Add...)
	gitIn(t, b, p.Remove...)
	gitIn(t, b, p.Commit...)

	files := gitIn(t, b, "show", "--name-status", "--format=%B", "HEAD")
	for _, want := range []string{"Add the page", "And the rest.", "A\t[id].tsx", "M\tshared.txt", "kept.txt", "D\tid.tsx"} {
		if !strings.Contains(files, want) {
			t.Errorf("commit is missing %q:\n%s", want, files)
		}
	}
	if strings.Contains(files, "other.txt") {
		t.Errorf("a file that wasn't chosen was committed:\n%s", files)
	}
	if st := strings.TrimSpace(gitIn(t, b, "status", "--porcelain")); st != "?? other.txt" {
		t.Errorf("status after the commit:\n%s", st)
	}

	// During a merge the message git prepared is offered.
	if msg := MergeMessage(ctx, b); msg != "" {
		t.Errorf("no merge, but MergeMessage = %q", msg)
	}
}
