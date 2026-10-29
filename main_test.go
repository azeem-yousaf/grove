package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/git"
)

// TestPlainConflicts runs --plain on a repository where feature/x and
// feature/y edit the same line of a.txt, and feature/z's uncommitted edit
// clashes with feature/y in b.txt. feature/x's edit of b.txt merges cleanly
// with both.
func TestPlainConflicts(t *testing.T) {
	if git.Available() != nil {
		t.Skip("git not installed")
	}
	for k, v := range map[string]string{
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t",
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
	} {
		t.Setenv(k, v)
	}
	root := t.TempDir()
	main := filepath.Join(root, "app")
	g := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g(root, "init", "-q", "-b", "main", main)
	write(filepath.Join(main, "a.txt"), "one\ntwo\nthree\nfour\nfive\nsix\n")
	write(filepath.Join(main, "b.txt"), "alpha\nbeta\ngamma\n")
	g(main, "add", ".")
	g(main, "commit", "-q", "-m", "initial")
	for _, b := range []string{"x", "y", "z"} {
		g(main, "worktree", "add", "-q", filepath.Join(root, b), "-b", "feature/"+b)
	}
	write(filepath.Join(root, "x", "a.txt"), "one\nTWO x\nthree\nfour\nfive\nsix\n")
	write(filepath.Join(root, "x", "b.txt"), "ALPHA\nbeta\ngamma\n")
	g(filepath.Join(root, "x"), "commit", "-q", "-am", "x")
	write(filepath.Join(root, "y", "a.txt"), "one\nTWO y\nthree\nfour\nfive\nSIX y\n")
	write(filepath.Join(root, "y", "b.txt"), "alpha\nbeta\nGAMMA y\n")
	g(filepath.Join(root, "y"), "commit", "-q", "-am", "y")
	write(filepath.Join(root, "z", "b.txt"), "alpha\nbeta\nGAMMA z\n")

	ctx := context.Background()
	repo, err := git.Open(ctx, main)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := git.MergeTreeSupported(ctx, repo.CommonDir); err != nil || !ok {
		t.Skip("git 2.38 or later needed")
	}
	out := captureStdout(t, func() error { return printPlain(ctx, repo, "main", git.ModeBranch, true) })
	_, section, ok := strings.Cut(out, "\nConflicts\n")
	if !ok {
		t.Fatalf("no Conflicts section:\n%s", out)
	}
	pairs := regexp.MustCompile(`(?m)^  (\S+) ↔ (\S+)$`).FindAllStringSubmatch(section, -1)
	var got []string
	for _, p := range pairs {
		names := []string{p[1], p[2]}
		if names[0] > names[1] {
			names[0], names[1] = names[1], names[0]
		}
		got = append(got, names[0]+" "+names[1])
	}
	slices.Sort(got)
	if strings.Join(got, ",") != "feature/x feature/y,feature/y feature/z" {
		t.Errorf("conflicting pairs = %v\n%s", got, out)
	}
	for _, want := range []string{`(?m)^    a\.txt +lines 2 \| lines 2$`, `(?m)^    b\.txt +lines 3 \| lines 3$`} {
		if !regexp.MustCompile(want).MatchString(section) {
			t.Errorf("missing %s in:\n%s", want, section)
		}
	}

	// Turned off in the config: no section.
	if out := captureStdout(t, func() error { return printPlain(ctx, repo, "main", git.ModeBranch, false) }); strings.Contains(out, "\nConflicts\n") {
		t.Errorf("prediction turned off but printed:\n%s", out)
	}
}

func captureStdout(t *testing.T, f func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	ferr := f()
	os.Stdout = old
	w.Close()
	out := <-done
	if ferr != nil {
		t.Fatal(ferr)
	}
	return out
}

func TestVersionFrom(t *testing.T) {
	settings := func(rev, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: rev}, {Key: "vcs.modified", Value: modified}}
	}
	for _, c := range []struct {
		ldflag string
		info   *debug.BuildInfo
		want   string
	}{
		{"0.3.0-abc1234", &debug.BuildInfo{Main: debug.Module{Version: "v9.9.9"}}, "0.3.0-abc1234"},
		{"dev", nil, "dev"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, "1.2.3"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261028215517-88b5d3f669ab+dirty"}}, "dev-88b5d3f-dirty"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261028215517-88b5d3f669ab"}}, "dev-88b5d3f"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings("88b5d3f669ab0000", "true")}, "dev-88b5d3f-dirty"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: settings("88b5d3f669ab0000", "false")}, "dev-88b5d3f"},
		{"dev", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "dev"},
	} {
		if got := versionFrom(c.ldflag, c.info); got != c.want {
			t.Errorf("versionFrom(%q, %+v) = %q, want %q", c.ldflag, c.info, got, c.want)
		}
	}
}
