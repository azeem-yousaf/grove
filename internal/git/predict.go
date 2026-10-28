package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// LineRange is a run of lines, 1-based and inclusive, in one side's version
// of a file.
type LineRange struct{ Start, End int }

// FileConflict is a file that would conflict when two worktrees are merged,
// with the lines each side changed that clash with the other's. A and B are
// nil for a side that deleted the file.
type FileConflict struct {
	Path string
	A, B []LineRange
}

// Side is one worktree as a merge input: its HEAD commit and a tree of
// everything in it, including uncommitted and untracked work.
type Side struct {
	Dir, Head, Tree string
	Clean           bool // Tree == Head^{tree}: nothing uncommitted or untracked
}

// Prediction is the predicted outcome of merging two worktrees.
type Prediction struct {
	A, B  int // indexes into Snapshot.States, A < B
	Files []FileConflict
	Clean bool // both sides had no uncommitted or untracked work
	Err   error
}

// MergeTreeSupported reports whether git is new enough (2.38) for
// `merge-tree --write-tree`, which prediction needs.
func MergeTreeSupported(ctx context.Context, dir string) (bool, error) {
	out, err := run(ctx, dir, "version")
	if err != nil {
		return false, err
	}
	major, minor, ok := parseGitVersion(string(out))
	if !ok {
		return false, fmt.Errorf("can't read git version from %q", strings.TrimSpace(string(out)))
	}
	return major > 2 || major == 2 && minor >= 38, nil
}

// parseGitVersion reads "git version 2.45.1.windows.1" and the like.
func parseGitVersion(s string) (major, minor int, ok bool) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return 0, 0, false
	}
	parts := strings.SplitN(f[2], ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	return major, minor, err1 == nil && err2 == nil
}

// SideOf turns a worktree's state into a merge input. A worktree with
// uncommitted or untracked files gets a tree written the way Checkpoint
// writes one, so the real index is never touched.
func SideOf(ctx context.Context, st State) (Side, error) {
	switch {
	case st.Bare:
		return Side{}, errors.New("bare repository")
	case st.Prunable:
		return Side{}, errors.New("worktree folder is gone")
	case st.Err != nil:
		return Side{}, st.Err
	case st.Head == "" || st.Rev == emptyTree:
		return Side{}, errors.New("no commits yet")
	}
	side := Side{Dir: st.Path, Head: st.Head, Clean: !st.Truncated}
	for _, f := range st.Files {
		if f.Uncommitted {
			side.Clean = false
			break
		}
	}
	if side.Clean {
		out, err := run(ctx, st.Path, "rev-parse", st.Head+"^{tree}")
		if err != nil {
			return Side{}, err
		}
		side.Tree = strings.TrimSpace(string(out))
		return side, nil
	}
	tree, err := Checkpoint(ctx, st.Path)
	if err != nil {
		return Side{}, err
	}
	side.Tree = tree
	return side, nil
}

// PredictConflicts merges a and b in git's object store only and lists the
// files that would conflict, with the clashing lines on each side. Sides
// with uncommitted work become throwaway commits first. Needs git 2.38.
func PredictConflicts(ctx context.Context, dir string, a, b Side) ([]FileConflict, error) {
	base, err := mergeBase(ctx, dir, a.Head, b.Head)
	if err != nil {
		return nil, err
	}
	return predict(ctx, dir, a, b, base)
}

func mergeBase(ctx context.Context, dir, a, b string) (string, error) {
	out, err := runCodes(ctx, dir, []int{1}, "merge-base", a, b)
	if err != nil {
		return "", err
	}
	base := strings.TrimSpace(string(out))
	if base == "" {
		return "", errors.New("no common history")
	}
	return base, nil
}

func predict(ctx context.Context, dir string, a, b Side, base string) ([]FileConflict, error) {
	ca, err := sideCommit(ctx, dir, a)
	if err != nil {
		return nil, err
	}
	cb, err := sideCommit(ctx, dir, b)
	if err != nil {
		return nil, err
	}
	_, paths, err := MergeConflicts(ctx, dir, ca, cb)
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	files := make([]FileConflict, 0, len(paths))
	for _, p := range paths {
		fc, err := fileConflict(ctx, dir, base, a.Tree, b.Tree, p)
		if err != nil {
			return nil, err
		}
		files = append(files, fc)
	}
	return files, nil
}

// sideCommit is the commit to merge for a side: its HEAD, or a throwaway
// commit of its tree on top of HEAD. Nothing refers to the commit, so gc
// removes it later.
func sideCommit(ctx context.Context, dir string, s Side) (string, error) {
	if s.Clean {
		return s.Head, nil
	}
	out, err := run(ctx, dir, "-c", "user.name=grove", "-c", "user.email=grove@localhost",
		"commit-tree", "--no-gpg-sign", s.Tree, "-p", s.Head, "-m", "grove conflict prediction")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// hunk is one @@ header of a -U0 diff.
type hunk struct{ oldStart, oldCount, newStart, newCount int }

// fileDiff is how one side changed a file since the merge base.
type fileDiff struct {
	hunks []hunk
	whole bool // added, deleted or binary: no line-level answer
}

func fileConflict(ctx context.Context, dir, base, treeA, treeB, path string) (FileConflict, error) {
	fc := FileConflict{Path: path}
	da, err := diffHunks(ctx, dir, base, treeA, path)
	if err != nil {
		return fc, err
	}
	db, err := diffHunks(ctx, dir, base, treeB, path)
	if err != nil {
		return fc, err
	}
	if da.whole || db.whole {
		if fc.A, err = wholeFile(ctx, dir, treeA, path); err != nil {
			return fc, err
		}
		fc.B, err = wholeFile(ctx, dir, treeB, path)
		return fc, err
	}
	for _, ha := range da.hunks {
		for _, hb := range db.hunks {
			if touches(ha, hb) {
				fc.A = append(fc.A, ha.newRange())
				fc.B = append(fc.B, hb.newRange())
			}
		}
	}
	// git's merge can line things up differently from two plain diffs;
	// when no pair of hunks meets, show every change on both sides.
	if fc.A == nil && fc.B == nil {
		for _, h := range da.hunks {
			fc.A = append(fc.A, h.newRange())
		}
		for _, h := range db.hunks {
			fc.B = append(fc.B, h.newRange())
		}
	}
	fc.A, fc.B = mergeRanges(fc.A), mergeRanges(fc.B)
	return fc, nil
}

// touches reports whether two hunks' merge-base ranges overlap or are next
// to each other. A pure insertion after line n sits between n and n+1.
func touches(a, b hunk) bool {
	alo, ahi := a.oldSpan()
	blo, bhi := b.oldSpan()
	return ahi+1 >= blo && bhi+1 >= alo
}

// oldSpan is the hunk's lines in the merge base. An insertion after line n
// is the empty span (n+1, n).
func (h hunk) oldSpan() (lo, hi int) {
	if h.oldCount == 0 {
		return h.oldStart + 1, h.oldStart
	}
	return h.oldStart, h.oldStart + h.oldCount - 1
}

// newRange is the hunk's lines in the side's file. A pure deletion is shown
// on the line before it.
func (h hunk) newRange() LineRange {
	if h.newCount == 0 {
		n := max(h.newStart, 1)
		return LineRange{n, n}
	}
	return LineRange{h.newStart, h.newStart + h.newCount - 1}
}

// mergeRanges sorts ranges and joins those that overlap or touch.
func mergeRanges(rs []LineRange) []LineRange {
	if len(rs) == 0 {
		return nil
	}
	rs = slices.Clone(rs)
	slices.SortFunc(rs, func(a, b LineRange) int { return a.Start - b.Start })
	out := rs[:1]
	for _, r := range rs[1:] {
		last := &out[len(out)-1]
		if r.Start <= last.End+1 {
			last.End = max(last.End, r.End)
			continue
		}
		out = append(out, r)
	}
	return out
}

func diffHunks(ctx context.Context, dir, base, tree, path string) (fileDiff, error) {
	out, err := run(ctx, dir, "--literal-pathspecs", "diff", "-U0", "--no-renames", "--no-ext-diff", "--no-textconv",
		base, tree, "--", path)
	if err != nil {
		return fileDiff{}, err
	}
	var d fileDiff
	for line := range bytes.Lines(out) {
		s := strings.TrimRight(string(line), "\r\n")
		switch {
		case strings.HasPrefix(s, "@@ "):
			if h, ok := parseHunk(s); ok {
				d.hunks = append(d.hunks, h)
			}
		case len(d.hunks) > 0:
			// Diff body; only headers before the first hunk matter below.
		case strings.HasPrefix(s, "new file mode"), strings.HasPrefix(s, "deleted file mode"),
			strings.HasPrefix(s, "Binary files"):
			d.whole = true
		}
	}
	return d, nil
}

// parseHunk reads "@@ -12,3 +12,4 @@ ..."; a missing count means 1.
func parseHunk(s string) (hunk, bool) {
	f := strings.Fields(s)
	if len(f) < 3 || !strings.HasPrefix(f[1], "-") || !strings.HasPrefix(f[2], "+") {
		return hunk{}, false
	}
	oldStart, oldCount, ok1 := parseSpan(f[1][1:])
	newStart, newCount, ok2 := parseSpan(f[2][1:])
	return hunk{oldStart, oldCount, newStart, newCount}, ok1 && ok2
}

func parseSpan(s string) (start, count int, ok bool) {
	a, b, has := strings.Cut(s, ",")
	start, err := strconv.Atoi(a)
	if err != nil {
		return 0, 0, false
	}
	if !has {
		return start, 1, true
	}
	count, err = strconv.Atoi(b)
	return start, count, err == nil
}

// wholeFile is one range covering path in tree, or nil if tree lacks it.
func wholeFile(ctx context.Context, dir, tree, path string) ([]LineRange, error) {
	b, err := run(ctx, dir, "cat-file", "blob", tree+":"+path)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Missing from this side (deleted), or not a file.
		return nil, nil
	}
	n := bytes.Count(b, []byte("\n"))
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return []LineRange{{1, max(n, 1)}}, nil
}

// Predictor predicts conflicts between every pair of worktrees that touch
// the same files, re-running only the pairs that changed. It is safe for
// concurrent use.
type Predictor struct {
	Workers int // pairs checked at once; 0 or less means 4

	mu        sync.Mutex
	cache     map[pairKey][]FileConflict
	bases     map[[2]string]string // merge-base by sorted pair of heads
	supported *bool
	runs      atomic.Int64 // pairs actually merged, for tests
}

// pairKey identifies a prediction by what it depends on. The trees are
// sorted, so the order of the two sides doesn't matter.
type pairKey struct{ treeA, treeB, base string }

func keyOf(treeA, treeB, base string) pairKey {
	if treeB < treeA {
		treeA, treeB = treeB, treeA
	}
	return pairKey{treeA, treeB, base}
}

// NewPredictor returns a Predictor with the default settings.
func NewPredictor() *Predictor { return &Predictor{Workers: 4} }

func (p *Predictor) isSupported(ctx context.Context, dir string) bool {
	p.mu.Lock()
	s := p.supported
	p.mu.Unlock()
	if s != nil {
		return *s
	}
	ok, err := MergeTreeSupported(ctx, dir)
	if err != nil {
		return false
	}
	p.mu.Lock()
	p.supported = &ok
	p.mu.Unlock()
	return ok
}

// Predict checks every pair of worktrees that share a path in
// snap.Overlaps and returns one Prediction per pair, conflicting or not,
// ordered by A then B. It returns nil when git is older than 2.38.
func (p *Predictor) Predict(ctx context.Context, r *Repo, snap *Snapshot) []Prediction {
	dir := r.CommonDir
	if snap == nil || len(snap.Overlaps) == 0 || !p.isSupported(ctx, dir) {
		p.keepOnly(nil, nil)
		return nil
	}
	pairs := map[[2]int]bool{}
	for _, idx := range snap.Overlaps {
		for i, a := range idx {
			for _, b := range idx[i+1:] {
				pairs[[2]int{min(a, b), max(a, b)}] = true
			}
		}
	}
	preds := make([]Prediction, 0, len(pairs))
	for pr := range pairs {
		preds = append(preds, Prediction{A: pr[0], B: pr[1]})
	}
	slices.SortFunc(preds, func(x, y Prediction) int {
		if x.A != y.A {
			return x.A - y.A
		}
		return x.B - y.B
	})

	workers := p.Workers
	if workers <= 0 {
		workers = 4
	}
	sem := make(chan struct{}, workers)

	// Each worktree's side once, however many pairs it is in.
	sides := map[int]*sideResult{}
	for _, pr := range preds {
		for _, i := range []int{pr.A, pr.B} {
			if sides[i] == nil {
				sides[i] = &sideResult{}
			}
		}
	}
	var wg sync.WaitGroup
	for i, sr := range sides {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			sr.side, sr.err = SideOf(ctx, snap.States[i])
		})
	}
	wg.Wait()

	usedKeys := map[pairKey]bool{}
	usedBases := map[[2]string]bool{}
	var usedMu sync.Mutex
	for i := range preds {
		pr := &preds[i]
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			a, b := sides[pr.A], sides[pr.B]
			if a.err != nil {
				pr.Err = fmt.Errorf("%s: %w", snap.States[pr.A].Name(), a.err)
				return
			}
			if b.err != nil {
				pr.Err = fmt.Errorf("%s: %w", snap.States[pr.B].Name(), b.err)
				return
			}
			pr.Clean = a.side.Clean && b.side.Clean
			heads := [2]string{min(a.side.Head, b.side.Head), max(a.side.Head, b.side.Head)}
			usedMu.Lock()
			usedBases[heads] = true
			usedMu.Unlock()
			base, err := p.mergeBase(ctx, dir, heads)
			if err != nil {
				pr.Err = err
				return
			}
			key := keyOf(a.side.Tree, b.side.Tree, base)
			usedMu.Lock()
			usedKeys[key] = true
			usedMu.Unlock()
			files, err := p.lookup(ctx, dir, key, a.side, b.side)
			pr.Files, pr.Err = orient(files, key, a.side.Tree), err
		})
	}
	wg.Wait()
	p.keepOnly(usedKeys, usedBases)
	return preds
}

type sideResult struct {
	side Side
	err  error
}

func (p *Predictor) mergeBase(ctx context.Context, dir string, heads [2]string) (string, error) {
	p.mu.Lock()
	base, ok := p.bases[heads]
	p.mu.Unlock()
	if ok {
		return base, nil
	}
	base, err := mergeBase(ctx, dir, heads[0], heads[1])
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	if p.bases == nil {
		p.bases = map[[2]string]string{}
	}
	p.bases[heads] = base
	p.mu.Unlock()
	return base, nil
}

// lookup returns the cached result for key, or merges a and b and caches
// it. The cached files are stored with A as key.treeA's side.
func (p *Predictor) lookup(ctx context.Context, dir string, key pairKey, a, b Side) ([]FileConflict, error) {
	p.mu.Lock()
	files, ok := p.cache[key]
	p.mu.Unlock()
	if ok {
		return files, nil
	}
	if a.Tree != key.treeA {
		a, b = b, a
	}
	p.runs.Add(1)
	files, err := predict(ctx, dir, a, b, key.base)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.cache == nil {
		p.cache = map[pairKey][]FileConflict{}
	}
	p.cache[key] = files
	p.mu.Unlock()
	return files, nil
}

// orient returns files with A as the side whose tree is treeA, swapping
// sides when the cache stored them the other way round.
func orient(files []FileConflict, key pairKey, treeA string) []FileConflict {
	if treeA == key.treeA || len(files) == 0 {
		return files
	}
	out := make([]FileConflict, len(files))
	for i, f := range files {
		out[i] = FileConflict{Path: f.Path, A: f.B, B: f.A}
	}
	return out
}

// keepOnly drops cache entries the last Predict call didn't use, so the
// cache holds at most one entry per current pair.
func (p *Predictor) keepOnly(keys map[pairKey]bool, bases map[[2]string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k := range p.cache {
		if !keys[k] {
			delete(p.cache, k)
		}
	}
	for k := range p.bases {
		if !bases[k] {
			delete(p.bases, k)
		}
	}
}

// Cached returns the conflicting paths the last Predict call found when
// merging source into target (commits or branches). ok is false unless
// that exact pair of trees and merge base is cached.
func (p *Predictor) Cached(ctx context.Context, dir, target, source string) (paths []string, ok bool) {
	out, err := run(ctx, dir, "rev-parse", target+"^{tree}", source+"^{tree}")
	if err != nil {
		return nil, false
	}
	trees := strings.Fields(string(out))
	if len(trees) != 2 {
		return nil, false
	}
	base, err := mergeBase(ctx, dir, target, source)
	if err != nil {
		return nil, false
	}
	p.mu.Lock()
	files, ok := p.cache[keyOf(trees[0], trees[1], base)]
	p.mu.Unlock()
	if !ok {
		return nil, false
	}
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	return paths, true
}
