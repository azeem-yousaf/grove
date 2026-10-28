package git

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// MergeConflicts asks Git's merge machinery which paths conflict when
// source is merged into target, and returns the tree the merge produces.
// Nothing in a worktree or index changes. It needs Git 2.38 or later.
func MergeConflicts(ctx context.Context, dir, target, source string) (tree string, paths []string, err error) {
	out, err := runCodes(ctx, dir, []int{1}, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", target, source)
	if err != nil {
		return "", nil, err
	}
	// The tree comes first, then one entry per conflicted path.
	fields := splitNUL(out)
	if len(fields) == 0 {
		return "", nil, nil
	}
	for _, p := range fields[1:] {
		if p != "" && !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	return fields[0], paths, nil
}

// MergeStep is one branch of a previewed sequence of merges.
type MergeStep struct {
	Source    string
	Conflicts []string
	Err       error
}

// PreviewMerges merges each source into target in order, the way Merge
// would one after another, but only in Git's object store: each clean
// result becomes a throwaway commit the next source is merged into. It
// stops after the first step that conflicts or fails, so later steps are
// missing from the result. No branch, worktree or index changes.
func PreviewMerges(ctx context.Context, dir, target string, sources []string) []MergeStep {
	base := target
	var steps []MergeStep
	for _, src := range sources {
		tree, paths, err := MergeConflicts(ctx, dir, base, src)
		step := MergeStep{Source: src, Conflicts: paths, Err: err}
		if err == nil && len(paths) == 0 {
			// The commit is never referenced, so gc removes it later.
			out, cerr := run(ctx, dir, "-c", "user.name=grove", "-c", "user.email=grove@localhost",
				"commit-tree", "--no-gpg-sign", tree, "-p", base, "-p", src, "-m", "grove merge preview")
			if cerr != nil {
				step.Err = cerr
			} else {
				base = strings.TrimSpace(string(out))
			}
		}
		steps = append(steps, step)
		if step.Err != nil || len(step.Conflicts) > 0 {
			break
		}
	}
	return steps
}

// CommitsBetween counts the commits reachable from to but not from.
func CommitsBetween(ctx context.Context, dir, from, to string) (int, error) {
	out, err := run(ctx, dir, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// Merge merges source into the branch checked out in dir. A merge that
// stops on conflicts is not an error: it returns the conflicted paths and
// leaves the merge in progress for someone to resolve. Callers must preview
// with MergeConflicts and ask the user first.
func Merge(ctx context.Context, dir, source string) ([]string, error) {
	_, err := run(ctx, dir, "merge", "--no-edit", source)
	if err == nil {
		return nil, nil
	}
	// git reports conflicts on stdout with a plain exit status 1, so ask
	// the index rather than reading the message.
	if paths, uerr := UnmergedPaths(ctx, dir); uerr == nil && len(paths) > 0 {
		return paths, nil
	}
	return nil, err
}

// UnmergedPaths lists the files with unresolved conflicts in dir.
func UnmergedPaths(ctx context.Context, dir string) ([]string, error) {
	out, err := run(ctx, dir, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	return splitNUL(out), nil
}

// MergeInProgress reports whether dir is in the middle of a merge.
func MergeInProgress(ctx context.Context, dir string) (bool, error) {
	out, err := runCodes(ctx, dir, []int{1}, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// AbortMerge puts dir back the way it was before the merge started.
func AbortMerge(ctx context.Context, dir string) error {
	_, err := run(ctx, dir, "merge", "--abort")
	return err
}

// HasTrackedChanges reports whether dir has staged or unstaged edits to
// tracked files. Untracked files don't count: a merge leaves them alone.
func HasTrackedChanges(ctx context.Context, dir string) (bool, error) {
	out, err := run(ctx, dir, "status", "--porcelain", "-z", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return len(out) > 0, nil
}
