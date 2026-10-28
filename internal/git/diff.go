package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Diff returns the unified diff of one file, against the same revision the
// file list in st was built from.
func Diff(ctx context.Context, st State, f FileChange) (string, error) {
	if f.Status == Untracked {
		// --no-index compares two paths outside the index; it exits 1 when
		// they differ, which is the expected outcome here. git special-cases
		// the literal "/dev/null" on every platform, including Windows.
		out, err := runCodes(ctx, st.Path, []int{1},
			"diff", "--no-index", "--no-color", "--no-ext-diff", "--", "/dev/null", f.Path)
		return string(out), err
	}
	args := []string{"diff", "--no-color", "--no-ext-diff", "-M", st.Rev, "--"}
	if f.OldPath != "" {
		args = append(args, f.OldPath)
	}
	args = append(args, f.Path)
	out, err := run(ctx, st.Path, args...)
	return string(out), err
}

// AbsPath is the on-disk location of a changed file.
func AbsPath(st State, f FileChange) string {
	return filepath.Join(st.Path, filepath.FromSlash(f.Path))
}

// DirOf returns the folder containing a changed file, walking up to the
// nearest folder that still exists (the file itself may have been deleted).
func DirOf(st State, f FileChange) string {
	dir := filepath.Dir(AbsPath(st, f))
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(strings.ToLower(parent), strings.ToLower(st.Path)) {
			return st.Path
		}
		dir = parent
	}
}
