package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Checkpoint records every file in the worktree at dir, tracked or not
// (ignored files excepted), as a git tree object. It works on a copy of the
// index, so the real index, the files and the branch are untouched. The
// tree lets an agent's changes be listed and undone later, and SideOf uses
// it to predict conflicts in work that isn't committed yet.
func Checkpoint(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "grove-index-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	// Starting from the real index makes `add -A` only hash what changed.
	if src, err := os.Open(nativePath(strings.TrimSpace(string(out)))); err == nil {
		_, err = io.Copy(tmp, src)
		src.Close()
		if err != nil {
			tmp.Close()
			return "", err
		}
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	env := call{env: []string{"GIT_INDEX_FILE=" + tmpPath}}
	// Store exact bytes: no CRLF conversion and no .gitattributes filters
	// (attributes are read from the empty tree), so undo restores files
	// byte for byte. --attr-source needs git 2.40; older gits skip it.
	exact := []string{"-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "add", "-A", "--", "."}
	if _, err := runWith(ctx, dir, env, append([]string{"--attr-source=" + emptyTree}, exact...)...); err != nil {
		var ge *Error
		if !errors.As(err, &ge) || !strings.Contains(ge.Stderr, "attr-source") {
			return "", err
		}
		if _, err := runWith(ctx, dir, env, exact...); err != nil {
			return "", err
		}
	}
	tree, err := runWith(ctx, dir, env, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(tree)), nil
}

// ChangeSince is a file that differs from a checkpoint.
type ChangeSince struct {
	Path   string // repo-relative, forward slashes
	Status Status // Added (new since the checkpoint), Modified or Deleted
}

// ChangedSince lists the files in dir that differ from the checkpoint tree.
func ChangedSince(ctx context.Context, dir, tree string) ([]ChangeSince, error) {
	now, err := Checkpoint(ctx, dir)
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, dir, "diff-tree", "-r", "-z", "--no-renames", "--name-status", tree, now)
	if err != nil {
		return nil, err
	}
	var list []ChangeSince
	for _, f := range parseNameStatus(out) {
		list = append(list, ChangeSince{Path: f.Path, Status: f.Status})
	}
	return list, nil
}

// RestoreCheckpoint puts the given files back the way they were in the
// checkpoint: changed and deleted files get their old content, files that
// didn't exist then are deleted. The index is left alone.
func RestoreCheckpoint(ctx context.Context, dir, tree string, changes []ChangeSince) error {
	var errs []error
	for _, c := range changes {
		p := filepath.Join(dir, filepath.FromSlash(c.Path))
		if c.Status == Added {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			removeEmptyParents(dir, filepath.Dir(p))
			continue
		}
		// cat-file applies no filters, so these are the checkpointed bytes.
		b, err := run(ctx, dir, "cat-file", "blob", tree+":"+c.Path)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(p), 0o755)
		}
		if err == nil {
			err = os.WriteFile(p, b, 0o644)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Path, err))
		}
	}
	return errors.Join(errs...)
}

// removeEmptyParents deletes folders left empty by removing a file, up to
// but not including root.
func removeEmptyParents(root, dir string) {
	for within(dir, root) && !SamePath(dir, root) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
