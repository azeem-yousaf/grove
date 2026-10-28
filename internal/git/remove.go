package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RemoveOptions control RemoveWorktree.
type RemoveOptions struct {
	// Force removes the worktree even when it has uncommitted or untracked
	// changes, or is locked. Those changes are lost.
	Force bool
	// DeleteBranch also deletes the worktree's branch, including commits
	// that aren't merged anywhere else.
	DeleteBranch bool
}

// RemoveResult describes what RemoveWorktree managed to do beyond the
// removal itself.
type RemoveResult struct {
	// FolderLeft is set when git forgot the worktree but its folder could not
	// be deleted, usually because another program still has it open.
	FolderLeft bool
	// BranchErr is why the branch couldn't be deleted, when it was asked for.
	BranchErr error
}

// RemoveWorktree deletes a linked worktree's folder and git's record of it.
// Without Force, git refuses when the worktree has changes that would be
// lost, so a stale view can't cause data loss.
func RemoveWorktree(ctx context.Context, r *Repo, wt Worktree, opt RemoveOptions) (RemoveResult, error) {
	var res RemoveResult
	if wt.Main || wt.Bare {
		return res, errors.New("the main worktree holds the repository and can't be removed")
	}

	// Windows can't delete a folder that is a process's current directory,
	// and git inherits ours, so step out of the worktree first.
	if cwd, err := os.Getwd(); err == nil && within(cwd, wt.Path) {
		_ = os.Chdir(filepath.Dir(wt.Path))
	}

	args := []string{"worktree", "remove"}
	if opt.Force {
		args = append(args, "--force")
		if wt.Locked {
			args = append(args, "--force") // git needs it twice to override a lock
		}
	}
	args = append(args, wt.Path)

	if _, err := run(ctx, r.CommonDir, args...); err != nil {
		// git deletes the files before it forgets the worktree, so when
		// another program holds the folder open it can fail on the last step
		// having otherwise succeeded.
		registered, lerr := isRegistered(ctx, r, wt.Path)
		if lerr != nil || registered {
			return res, explainRemoveErr(err)
		}
		res.FolderLeft = !removeLeftovers(ctx, wt.Path)
	}

	if opt.DeleteBranch && wt.Branch != "" {
		if _, err := run(ctx, r.CommonDir, "branch", "-D", wt.Branch); err != nil {
			res.BranchErr = err
		}
	}
	return res, nil
}

func isRegistered(ctx context.Context, r *Repo, path string) (bool, error) {
	out, err := run(ctx, r.CommonDir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	for _, w := range parseWorktrees(out) {
		if SamePath(w.Path, path) {
			return true, nil
		}
	}
	return false, nil
}

// removeLeftovers retries deleting what git couldn't, giving editors and
// file watchers a moment to let go. It reports whether the folder is gone.
func removeLeftovers(ctx context.Context, path string) bool {
	for i := 0; i < 5; i++ {
		if err := os.RemoveAll(path); err == nil {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Duration(i+1) * 200 * time.Millisecond):
		}
	}
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

// explainRemoveErr turns git's refusals into advice.
func explainRemoveErr(err error) error {
	var ge *Error
	if !errors.As(err, &ge) {
		return err
	}
	msg := ge.Stderr
	switch {
	case strings.Contains(msg, "modified or untracked files"):
		return errors.New("it has uncommitted changes; choose to discard them to remove it anyway")
	case strings.Contains(msg, "locked working tree"):
		return errors.New("it is locked; choose to override the lock to remove it anyway")
	case strings.Contains(msg, "Permission denied"), strings.Contains(msg, "busy"), strings.Contains(msg, "Invalid argument"):
		return errors.New("some files are in use; close editors and terminals open in that folder and try again")
	}
	return err
}

// within reports whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if SamePath(path, dir) {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
