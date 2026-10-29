package git

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
)

// CommitWork commits everything in the worktree at dir, new files
// included, except the paths in exclude (relative to dir, with forward
// slashes), such as files copied in from the main worktree. It reports
// whether there was anything to commit.
func CommitWork(ctx context.Context, dir, message string, exclude []string) (bool, error) {
	if err := stageAll(ctx, dir, exclude); err != nil {
		return false, err
	}
	out, err := run(ctx, dir, "diff", "--cached", "--name-only", "-z")
	if err != nil || len(out) == 0 {
		return false, err
	}
	if _, err := run(ctx, dir, "commit", "-q", "-m", message); err != nil {
		return false, err
	}
	return true, nil
}

// stageAll stages every change in dir but the excluded paths.
func stageAll(ctx context.Context, dir string, exclude []string) error {
	specs := []string{"."}
	for _, p := range exclude {
		specs = append(specs, ":(exclude,literal)"+p)
	}
	in := strings.NewReader(strings.Join(specs, "\x00") + "\x00")
	_, err := runWith(ctx, dir, call{stdin: in}, "add", "--all", "--pathspec-from-file=-", "--pathspec-file-nul")
	return err
}

// ConcludeMerge commits the merge in progress in dir once someone has
// resolved its conflicts. While files still hold conflict markers it
// returns them and commits nothing. Without a merge in progress it does
// nothing.
func ConcludeMerge(ctx context.Context, dir string, exclude []string) ([]string, error) {
	if busy, err := MergeInProgress(ctx, dir); err != nil || !busy {
		return nil, err
	}
	paths, err := UnmergedPaths(ctx, dir)
	if err != nil {
		return nil, err
	}
	var left []string
	for _, p := range paths {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err == nil && hasConflictMarkers(b) {
			left = append(left, p) // a file that's gone was resolved by deleting it
		}
	}
	if len(left) > 0 {
		return left, nil
	}
	if err := stageAll(ctx, dir, exclude); err != nil {
		return nil, err
	}
	_, err = run(ctx, dir, "commit", "-q", "--no-edit")
	return nil, err
}

// hasConflictMarkers reports whether b has a line git starts or ends a
// conflict with.
func hasConflictMarkers(b []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		l := sc.Bytes()
		if bytes.HasPrefix(l, []byte("<<<<<<< ")) || bytes.HasPrefix(l, []byte(">>>>>>> ")) {
			return true
		}
	}
	return false
}
