package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Repo is the repository grove was pointed at.
type Repo struct {
	// Here is the top level of the worktree grove was launched from.
	Here string
	// CommonDir is the .git directory shared by every worktree.
	CommonDir string
	// Name is a display name for the repository.
	Name string
}

// Open resolves the repository containing path. path can be a worktree, any
// folder or file inside one, a .git folder (including a linked worktree's
// .git/worktrees/<name>), or a bare repository.
func Open(ctx context.Context, path string) (*Repo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("%s does not exist", path)
	}
	if !fi.IsDir() {
		abs = filepath.Dir(abs)
	}

	out, err := run(ctx, abs, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-toplevel")
	if err == nil {
		lines := splitLines(out)
		if len(lines) == 2 {
			common := nativePath(lines[0])
			return &Repo{CommonDir: common, Here: nativePath(lines[1]), Name: repoName(common)}, nil
		}
	}
	if errors.Is(err, ErrNotRepo) {
		return nil, err
	}

	// Inside a .git folder or a bare repository there is no top level, so
	// work out which worktree, if any, owns the git dir we were given.
	out, err = run(ctx, abs, "rev-parse", "--path-format=absolute", "--git-common-dir", "--absolute-git-dir", "--is-bare-repository")
	if err != nil {
		return nil, err
	}
	lines := splitLines(out)
	if len(lines) != 3 {
		return nil, fmt.Errorf("unexpected output from git rev-parse: %q", out)
	}
	common, gitDir := nativePath(lines[0]), nativePath(lines[1])
	return &Repo{
		CommonDir: common,
		Here:      worktreeOf(gitDir, common, lines[2] == "true"),
		Name:      repoName(common),
	}, nil
}

// worktreeOf returns the worktree folder that owns gitDir, or "" for a bare
// repository.
func worktreeOf(gitDir, common string, bare bool) string {
	if SamePath(gitDir, common) {
		if bare || !strings.EqualFold(filepath.Base(common), ".git") {
			return ""
		}
		return filepath.Dir(common)
	}
	// A linked worktree's git dir records where its checkout lives in a
	// "gitdir" file pointing at <worktree>/.git.
	b, err := os.ReadFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return ""
	}
	wt := filepath.Dir(nativePath(strings.TrimSpace(string(b))))
	if _, err := os.Stat(wt); err != nil {
		return ""
	}
	return wt
}

func splitLines(b []byte) []string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return lines
}

// repoName derives a friendly name from the common dir: /src/app/.git -> app,
// /src/app.git -> app.
func repoName(commonDir string) string {
	base := filepath.Base(commonDir)
	if strings.EqualFold(base, ".git") {
		return filepath.Base(filepath.Dir(commonDir))
	}
	return strings.TrimSuffix(base, ".git")
}
