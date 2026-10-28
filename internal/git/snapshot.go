package git

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Snapshot is the state of every worktree in a repository at one moment.
type Snapshot struct {
	Base   string
	Mode   Mode
	States []State
	// Overlaps maps a file path to the indexes (into States) of every
	// worktree that changed it, for paths changed in two or more worktrees.
	Overlaps map[string][]int
	Taken    time.Time
	Elapsed  time.Duration
}

// Scan inspects every worktree of the repository concurrently.
func Scan(ctx context.Context, r *Repo, base string, mode Mode) (*Snapshot, error) {
	start := time.Now()
	wts, err := Worktrees(ctx, r)
	if err != nil {
		return nil, err
	}
	states := make([]State, len(wts))
	var wg sync.WaitGroup
	for i, wt := range wts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			states[i] = Inspect(ctx, wt, base, mode)
		}()
	}
	wg.Wait()
	return &Snapshot{
		Base:     base,
		Mode:     mode,
		States:   states,
		Overlaps: overlaps(states),
		Taken:    time.Now(),
		Elapsed:  time.Since(start),
	}, nil
}

func overlaps(states []State) map[string][]int {
	seen := map[string][]int{}
	for i, st := range states {
		for _, f := range st.Files {
			seen[f.Path] = append(seen[f.Path], i)
		}
	}
	for p, idx := range seen {
		if len(idx) < 2 {
			delete(seen, p)
		}
	}
	return seen
}

// OverlapCount is the number of distinct files changed in more than one
// worktree.
func (s *Snapshot) OverlapCount() int { return len(s.Overlaps) }

// IndexOf returns the index of the worktree at path, or -1.
func (s *Snapshot) IndexOf(path string) int {
	for i, st := range s.States {
		if samePath(st.Path, path) {
			return i
		}
	}
	return -1
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// SamePath reports whether two paths refer to the same location, ignoring
// case on Windows.
func SamePath(a, b string) bool { return samePath(a, b) }
