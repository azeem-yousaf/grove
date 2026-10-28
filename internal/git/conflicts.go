package git

import (
	"context"
	"strings"
)

// MergeConflicts asks Git's merge machinery which paths conflict when the
// source commit is merged into the target commit. It does not change a
// worktree or index.
func MergeConflicts(ctx context.Context, dir, target, source string) ([]string, error) {
	out, err := runCodes(ctx, dir, []int{1}, "merge-tree", "--write-tree", "--name-only", target, source)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) <= 1 || lines[0] == "" {
		return nil, nil
	}
	// The first line is the tentative tree hash. Conflict names follow it.
	return lines[1:], nil
}

// Merge merges source into the checked-out branch in dir. Callers must first
// preview with MergeConflicts and ask the user before invoking this operation.
func Merge(ctx context.Context, dir, source string) error {
	_, err := run(ctx, dir, "merge", "--no-edit", source)
	return err
}
