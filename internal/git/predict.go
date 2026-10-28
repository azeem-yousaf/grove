package git

// Placeholder for feature/predict-git's predict.go, so this branch builds on
// its own: the same types, nothing else. On merge, take that branch's file.

// LineRange is lines Start to End, 1-based and inclusive, in one side's
// version of a file.
type LineRange struct{ Start, End int }

// FileConflict is a file that would conflict, with the lines involved on
// each side; nil means that side deleted the file.
type FileConflict struct {
	Path string
	A, B []LineRange
}

// Side is one worktree's state as a merge input.
type Side struct {
	Dir, Head, Tree string
	Clean           bool
}

// Prediction is what merging the worktrees at Snapshot.States indexes A
// and B would do.
type Prediction struct {
	A, B  int
	Files []FileConflict
	Clean bool
	Err   error
}
