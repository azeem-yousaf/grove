package gitguard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for grove: the hook scripts
// Install writes run os.Executable() as `<exe> git-guard <hook> ...`.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "git-guard" {
		os.Exit(Hook(os.Args[2], os.Args[3:], os.Stdin, os.Stderr, os.Environ()))
	}
	os.Exit(m.Run())
}

type fixture struct {
	t      *testing.T
	main   string // on master
	a, b   string // feature/a, feature/b
	remote string // a bare repository with master
	common string
	store  string
	g      Guard
}

// testRepo builds master plus feature/a and feature/b worktrees and a bare
// remote, and installs the guard protecting master.
func testRepo(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	out, err := exec.Command("git", "version").Output()
	if err != nil || !versionAtLeast(string(out), 2, 31) {
		t.Skipf("git too old for the guard: %s", out)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	root := t.TempDir()
	f := &fixture{
		t:      t,
		main:   filepath.Join(root, "app"),
		a:      filepath.Join(root, "app-a"),
		b:      filepath.Join(root, "app-b"),
		remote: filepath.Join(root, "remote.git"),
		store:  filepath.Join(root, "store"),
	}
	f.common = filepath.Join(f.main, ".git")
	if err := os.MkdirAll(f.main, 0o755); err != nil {
		t.Fatal(err)
	}
	f.ok(false, f.main, "init", "-q", "-b", "master")
	f.ok(false, f.main, "commit", "-q", "--allow-empty", "-m", "initial")
	f.ok(false, f.main, "worktree", "add", "-q", f.a, "-b", "feature/a")
	f.ok(false, f.main, "worktree", "add", "-q", f.b, "-b", "feature/b")
	f.ok(false, root, "init", "-q", "--bare", f.remote)
	f.ok(false, f.main, "push", "-q", f.remote, "master")
	// git itself refuses `branch -f` of a branch checked out elsewhere, which
	// would hide the guard's answer.
	f.ok(false, f.main, "checkout", "-q", "--detach")
	f.install()
	return f
}

func (f *fixture) install() {
	f.t.Helper()
	g, err := Install(f.store, f.common, []string{"master", "release/*"})
	if err != nil {
		f.t.Fatal(err)
	}
	f.g = g
}

// git runs git in dir, under the guard when guarded.
func (f *fixture) git(guarded bool, dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = os.Environ()
	if guarded {
		cmd.Env = append(cmd.Env, f.g.Env()...)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *fixture) ok(guarded bool, dir string, args ...string) string {
	f.t.Helper()
	out, err := f.git(guarded, dir, args...)
	if err != nil {
		f.t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

// refused expects git to fail with the guard's message for ref.
func (f *fixture) refused(dir, ref string, args ...string) {
	f.t.Helper()
	out, err := f.git(true, dir, args...)
	if err == nil {
		f.t.Fatalf("git %s in %s went through; want it refused", strings.Join(args, " "), dir)
	}
	if !strings.Contains(out, refusal(ref)) {
		f.t.Fatalf("git %s in %s failed without the guard's message:\n%s", strings.Join(args, " "), dir, out)
	}
}

func (f *fixture) rev(dir, rev string) string {
	f.t.Helper()
	return strings.TrimSpace(f.ok(false, dir, "rev-parse", rev))
}

func TestGuardRefusesProtectedUpdates(t *testing.T) {
	f := testRepo(t)
	master := f.rev(f.main, "master")
	for _, wt := range []string{f.a, f.b} {
		f.ok(true, wt, "commit", "-q", "--allow-empty", "-m", "work")
		f.refused(wt, "refs/heads/master", "branch", "-f", "master", "HEAD")
		f.refused(wt, "refs/heads/master", "update-ref", "refs/heads/master", "HEAD")
		f.refused(wt, "refs/heads/master", "update-ref", "-d", "refs/heads/master")
		f.refused(wt, "refs/heads/release/1", "branch", "release/1")
		f.refused(wt, "refs/heads/master", "push", f.remote, "HEAD:master")
		f.ok(true, wt, "push", "-q", f.remote, "HEAD")

		branch := strings.TrimSpace(f.ok(false, wt, "branch", "--show-current"))
		f.ok(true, wt, "checkout", "-q", "--ignore-other-worktrees", "master")
		f.refused(wt, "refs/heads/master", "commit", "--allow-empty", "-m", "on master")
		f.ok(true, wt, "checkout", "-q", "-f", branch)
	}
	f.ok(true, f.main, "checkout", "-q", "master")
	f.refused(f.main, "refs/heads/master", "commit", "--allow-empty", "-m", "on master")
	if got := f.rev(f.main, "master"); got != master {
		t.Fatalf("master moved to %s", got)
	}
	if got := f.rev(f.remote, "master"); got != master {
		t.Fatalf("the remote's master moved to %s", got)
	}
}

func TestGuardPushRules(t *testing.T) {
	f := testRepo(t)
	f.ok(true, f.a, "commit", "-q", "--allow-empty", "-m", "one")
	f.ok(true, f.a, "push", "-q", f.remote, "HEAD:feature/a")
	f.ok(true, f.a, "commit", "-q", "--amend", "--allow-empty", "-m", "rewritten")
	f.refused(f.a, "refs/heads/feature/a", "push", "-f", f.remote, "HEAD:feature/a")
	f.refused(f.a, "refs/heads/feature/a", "push", f.remote, ":feature/a")

	if _, err := f.g.Grant("feature/a", f.a, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.ok(true, f.a, "push", "-q", "-f", f.remote, "HEAD:feature/a")

	if _, err := f.g.Grant("master", f.a, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.ok(true, f.a, "push", "-q", f.remote, "HEAD:master")
	if f.rev(f.remote, "master") != f.rev(f.a, "HEAD") {
		t.Fatal("the granted push did not update the remote's master")
	}
}

func TestGrant(t *testing.T) {
	f := testRepo(t)
	f.ok(true, f.a, "commit", "-q", "--allow-empty", "-m", "work")

	// A grant for another worktree doesn't match.
	if _, err := f.g.Grant("master", f.b, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.refused(f.a, "refs/heads/master", "branch", "-f", "master", "HEAD")

	// One grant, one update.
	if _, err := f.g.Grant("refs/heads/master", f.a, time.Minute); err != nil {
		t.Fatal(err)
	}
	f.ok(true, f.a, "branch", "-f", "master", "HEAD")
	if f.rev(f.a, "master") != f.rev(f.a, "HEAD") {
		t.Fatal("the granted update did not move master")
	}
	f.ok(true, f.a, "commit", "-q", "--allow-empty", "-m", "more")
	f.refused(f.a, "refs/heads/master", "branch", "-f", "master", "HEAD")

	// An expired grant doesn't count.
	if _, err := f.g.Grant("master", f.a, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	f.refused(f.a, "refs/heads/master", "branch", "-f", "master", "HEAD")

	// The other worktree's grant is still there.
	f.ok(true, f.b, "branch", "-f", "master", "feature/a")
}

func TestUnguardedGitIsUntouched(t *testing.T) {
	f := testRepo(t)
	f.ok(false, f.a, "commit", "-q", "--allow-empty", "-m", "work")
	f.ok(false, f.a, "branch", "-f", "master", "HEAD")
	f.ok(false, f.a, "push", "-q", f.remote, "HEAD:master")
	f.ok(false, f.main, "checkout", "-q", "master")
	f.ok(false, f.main, "commit", "-q", "--allow-empty", "-m", "on master")
	if out, err := f.git(false, f.main, "config", "--show-origin", "--get-all", "core.hooksPath"); err == nil {
		t.Fatalf("Install set core.hooksPath: %s", out)
	}
}

func TestOriginalHooksStillRun(t *testing.T) {
	f := testRepo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	block := filepath.Join(t.TempDir(), "block")
	hook := "#!/bin/sh\necho ran >> " + shQuote(marker) + "\nif [ -e " + shQuote(block) + " ]; then echo blocked by pre-commit >&2; exit 1; fi\nexit 0\n"
	writeHook(t, filepath.Join(f.common, "hooks", "pre-commit"), hook)
	f.install()

	f.ok(true, f.a, "commit", "-q", "--allow-empty", "-m", "work")
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the repository's pre-commit hook did not run under the guard")
	}
	if err := os.WriteFile(block, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := f.git(true, f.a, "commit", "--allow-empty", "-m", "blocked")
	if err == nil || !strings.Contains(out, "blocked by pre-commit") {
		t.Fatalf("a failing pre-commit hook did not block the commit: %v\n%s", err, out)
	}

	// The user's own reference-transaction hook runs after the guard's check.
	rt := filepath.Join(t.TempDir(), "rt")
	writeHook(t, filepath.Join(f.common, "hooks", "reference-transaction"),
		"#!/bin/sh\ncat >> "+shQuote(rt)+"\n")
	os.Remove(block)
	f.install()
	f.ok(true, f.a, "commit", "-q", "--allow-empty", "-m", "again")
	if data, _ := os.ReadFile(rt); !strings.Contains(string(data), "refs/heads/feature/a") {
		t.Fatalf("the user's reference-transaction hook did not get the updates: %q", data)
	}

	// A hook removed from the original folder goes from the guard's too.
	os.Remove(filepath.Join(f.common, "hooks", "pre-commit"))
	f.install()
	if _, err := os.Stat(filepath.Join(f.g.Dir, "pre-commit")); !os.IsNotExist(err) {
		t.Fatal("the forwarding stub outlived the original hook")
	}
}

func TestUserHooksPathHonoured(t *testing.T) {
	f := testRepo(t)
	own := filepath.Join(t.TempDir(), "my hooks")
	writeHook(t, filepath.Join(own, "pre-commit"), "#!/bin/sh\necho from my hooks >&2\nexit 1\n")
	f.ok(false, f.main, "config", "core.hooksPath", own)
	f.install()
	out, err := f.git(true, f.a, "commit", "--allow-empty", "-m", "x")
	if err == nil || !strings.Contains(out, "from my hooks") {
		t.Fatalf("the user's core.hooksPath was not honoured: %v\n%s", err, out)
	}
	f.refused(f.a, "refs/heads/master", "branch", "-f", "master", "HEAD")
}

func TestVerify(t *testing.T) {
	f := testRepo(t)
	ctx := context.Background()
	for _, wt := range []string{f.main, f.a, f.b} {
		if err := Verify(ctx, f.g, wt); err != nil {
			t.Errorf("Verify in %s: %v", wt, err)
		}
	}
	none := Guard{Dir: t.TempDir()}
	if err := Verify(ctx, none, f.a); err == nil {
		t.Fatal("Verify passed without the hook")
	}
	if _, err := f.git(false, f.a, "rev-parse", "--verify", "-q", ProbeRef); err == nil {
		t.Fatal("Verify left the probe ref behind")
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	f := testRepo(t)
	p := filepath.Join(f.g.Dir, "reference-transaction")
	before, _ := os.Stat(p)
	time.Sleep(10 * time.Millisecond)
	f.install()
	after, _ := os.Stat(p)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("Install rewrote an unchanged hook")
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "\r") {
		t.Fatal("hook script has CR line endings")
	}
}

func TestEnvNumbering(t *testing.T) {
	g := Guard{Dir: "/x"}
	t.Setenv("GIT_CONFIG_COUNT", "2")
	got := strings.Join(g.Env(), " ")
	want := "GIT_CONFIG_COUNT=3 GIT_CONFIG_KEY_2=core.hooksPath GIT_CONFIG_VALUE_2=/x"
	if got != want {
		t.Fatalf("Env = %s; want %s", got, want)
	}
	os.Unsetenv("GIT_CONFIG_COUNT")
	if got := g.Env()[0]; got != "GIT_CONFIG_COUNT=1" {
		t.Fatalf("Env()[0] = %s", got)
	}
}

func writeHook(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}
