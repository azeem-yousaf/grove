package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode decides what counts as a change.
type Mode int

const (
	// ModeBranch shows everything a worktree changed since it left the base
	// branch: committed work plus anything not yet committed.
	ModeBranch Mode = iota
	// ModeUncommitted shows only work that is not committed yet.
	ModeUncommitted
)

func (m Mode) String() string {
	if m == ModeUncommitted {
		return "uncommitted"
	}
	return "branch"
}

// ParseMode accepts "branch" or "uncommitted".
func ParseMode(s string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "branch":
		return ModeBranch, nil
	case "uncommitted", "working":
		return ModeUncommitted, nil
	}
	return ModeBranch, errors.New(`mode must be "branch" or "uncommitted"`)
}

// Status is a one-letter change kind, matching git's own letters.
type Status byte

const (
	Added       Status = 'A'
	Modified    Status = 'M'
	Deleted     Status = 'D'
	Renamed     Status = 'R'
	Copied      Status = 'C'
	TypeChanged Status = 'T'
	Conflicted  Status = 'U'
	Untracked   Status = '?'
)

// Label is a human word for the status.
func (s Status) Label() string {
	switch s {
	case Added:
		return "added"
	case Modified:
		return "modified"
	case Deleted:
		return "deleted"
	case Renamed:
		return "renamed"
	case Copied:
		return "copied"
	case TypeChanged:
		return "type changed"
	case Conflicted:
		return "conflicted"
	case Untracked:
		return "untracked"
	}
	return "changed"
}

// FileChange is one changed file in a worktree. Paths are repo-relative with
// forward slashes, as git prints them.
type FileChange struct {
	Path    string
	OldPath string // set for renames and copies
	Status  Status
	Added   int
	Deleted int
	Binary  bool
	// Uncommitted is true when the file has staged or unstaged edits, as
	// opposed to changes that only exist in commits.
	Uncommitted bool
}

// Commit is a short description of a commit.
type Commit struct {
	Hash    string
	Subject string
	Author  string
	When    time.Time
}

// State is everything grove knows about one worktree at one moment.
type State struct {
	Worktree
	Base      string // branch compared against, "" when there is none
	Rev       string // revision the file list is diffed against
	Ahead     int    // commits on this worktree not in Base
	Behind    int    // commits in Base not on this worktree
	Files     []FileChange
	Added     int
	Deleted   int
	Last      *Commit  // Recent[0]
	Recent    []Commit // the newest few commits on HEAD, newest first
	Truncated bool // too many untracked files; the list is partial
	Err       error
}

// emptyTree is git's well-known hash of the empty tree, used to diff a branch
// that has no commits yet.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

const (
	maxUntracked      = 1000
	maxCountLineBytes = 8 << 20
)

// Inspect gathers the changed files of one worktree.
func Inspect(ctx context.Context, wt Worktree, base string, mode Mode) State {
	st := State{Worktree: wt}
	switch {
	case wt.Prunable:
		st.Err = errors.New("worktree folder is missing")
		return st
	case wt.Bare:
		return st
	}
	if _, err := os.Stat(wt.Path); err != nil {
		st.Err = errors.New("worktree folder is missing")
		return st
	}

	var (
		wg        sync.WaitGroup
		status    []statusEntry
		statusErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		status, statusErr = readStatus(ctx, wt.Path)
	}()
	go func() {
		defer wg.Done()
		st.Recent = recentCommits(ctx, wt.Path, recentCount)
		if len(st.Recent) > 0 {
			st.Last = &st.Recent[0]
		}
	}()

	hasHead := wt.Head != "" && refExists(ctx, wt.Path, "HEAD")
	st.Rev = "HEAD"
	if !hasHead {
		st.Rev = emptyTree
	}
	if base != "" && hasHead {
		st.Base = base
		if mode == ModeBranch {
			if out, err := run(ctx, wt.Path, "merge-base", base, "HEAD"); err == nil {
				st.Rev = strings.TrimSpace(string(out))
			}
		}
		if out, err := run(ctx, wt.Path, "rev-list", "--left-right", "--count", base+"...HEAD"); err == nil {
			f := strings.Fields(string(out))
			if len(f) == 2 {
				st.Behind, _ = strconv.Atoi(f[0])
				st.Ahead, _ = strconv.Atoi(f[1])
			}
		}
	}

	files, err := diffFiles(ctx, wt.Path, st.Rev)
	wg.Wait()
	if err == nil {
		err = statusErr
	}
	if err != nil {
		st.Err = err
		return st
	}

	byPath := make(map[string]int, len(files))
	for i, f := range files {
		byPath[f.Path] = i
		if mode == ModeUncommitted {
			files[i].Uncommitted = true
		}
	}
	untracked := 0
	for _, e := range status {
		if e.kind == '?' {
			if untracked == maxUntracked {
				st.Truncated = true
				continue
			}
			untracked++
			fc := FileChange{Path: e.path, Status: Untracked, Uncommitted: true}
			fc.Added, fc.Binary = countLines(filepath.Join(wt.Path, filepath.FromSlash(e.path)))
			files = append(files, fc)
			continue
		}
		if i, ok := byPath[e.path]; ok {
			files[i].Uncommitted = true
			if e.kind == 'u' {
				files[i].Status = Conflicted
			}
		}
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		st.Added += f.Added
		st.Deleted += f.Deleted
	}
	st.Files = files
	return st
}

// diffFiles lists files that differ between rev and the working tree,
// including staged and unstaged edits.
func diffFiles(ctx context.Context, dir, rev string) ([]FileChange, error) {
	var (
		wg               sync.WaitGroup
		numstat, names   []byte
		numErr, namesErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		numstat, numErr = run(ctx, dir, "diff", "--numstat", "-z", "-M", "--no-ext-diff", rev, "--")
	}()
	go func() {
		defer wg.Done()
		names, namesErr = run(ctx, dir, "diff", "--name-status", "-z", "-M", "--no-ext-diff", rev, "--")
	}()
	wg.Wait()
	if err := errors.Join(numErr, namesErr); err != nil {
		return nil, err
	}
	files := parseNameStatus(names)
	counts := parseNumstat(numstat)
	for i := range files {
		if c, ok := counts[files[i].Path]; ok {
			files[i].Added, files[i].Deleted, files[i].Binary = c.added, c.deleted, c.binary
		}
	}
	return files, nil
}

type lineCount struct {
	added, deleted int
	binary         bool
}

// parseNumstat parses `git diff --numstat -z`. A normal record is
// "added\tdeleted\tpath\0"; a rename is "added\tdeleted\t\0old\0new\0".
// Binary files report "-" for both counts.
func parseNumstat(b []byte) map[string]lineCount {
	out := map[string]lineCount{}
	fields := splitNUL(b)
	for i := 0; i < len(fields); i++ {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		if path == "" && i+2 < len(fields) {
			path = fields[i+2]
			i += 2
		}
		var c lineCount
		if parts[0] == "-" {
			c.binary = true
		} else {
			c.added, _ = strconv.Atoi(parts[0])
			c.deleted, _ = strconv.Atoi(parts[1])
		}
		out[path] = c
	}
	return out
}

// parseNameStatus parses `git diff --name-status -z`: a status field followed
// by one path, or two paths for renames and copies (R100, C075, ...).
func parseNameStatus(b []byte) []FileChange {
	var files []FileChange
	fields := splitNUL(b)
	for i := 0; i < len(fields); i++ {
		code := fields[i]
		if code == "" || i+1 >= len(fields) {
			continue
		}
		fc := FileChange{Status: Status(code[0])}
		if (fc.Status == Renamed || fc.Status == Copied) && i+2 < len(fields) {
			fc.OldPath, fc.Path = fields[i+1], fields[i+2]
			i += 2
		} else {
			fc.Path = fields[i+1]
			i++
		}
		files = append(files, fc)
	}
	return files
}

type statusEntry struct {
	kind byte // '1' ordinary, '2' rename/copy, 'u' unmerged, '?' untracked
	path string
}

func readStatus(ctx context.Context, dir string) ([]statusEntry, error) {
	out, err := run(ctx, dir, "status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignore-submodules=dirty")
	if err != nil {
		return nil, err
	}
	return parseStatus(out), nil
}

// parseStatus parses `git status --porcelain=v2 -z`. Paths can contain
// spaces, so each record is split into a fixed number of fields with the
// path last.
func parseStatus(b []byte) []statusEntry {
	var entries []statusEntry
	fields := splitNUL(b)
	for i := 0; i < len(fields); i++ {
		rec := fields[i]
		if rec == "" {
			continue
		}
		var n int
		switch rec[0] {
		case '1':
			n = 9
		case '2':
			n = 10
		case 'u':
			n = 11
		case '?':
			entries = append(entries, statusEntry{kind: '?', path: rec[2:]})
			continue
		default: // '!' ignored, '#' headers
			continue
		}
		parts := strings.SplitN(rec, " ", n)
		if len(parts) != n {
			continue
		}
		entries = append(entries, statusEntry{kind: rec[0], path: parts[n-1]})
		if rec[0] == '2' {
			i++ // skip the original path of the rename
		}
	}
	return entries
}

// countLines counts lines in an untracked file, reporting binary files
// (anything with a NUL byte near the start) separately.
func countLines(path string) (int, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	r := bufio.NewReaderSize(io.LimitReader(f, maxCountLineBytes), 64<<10)
	head, _ := r.Peek(8000)
	if bytes.IndexByte(head, 0) >= 0 {
		return 0, true
	}
	lines, last := 0, byte('\n')
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			lines += bytes.Count(buf[:n], []byte{'\n'})
			last = buf[n-1]
		}
		if err != nil {
			break
		}
	}
	if last != '\n' {
		lines++
	}
	return lines, false
}

// recentCount is how many commits State.Recent keeps.
const recentCount = 5

// recentCommits lists the newest n commits on HEAD, newest first. Each
// record ends in NUL and its fields are split by 0x1f, which no subject or
// name contains.
func recentCommits(ctx context.Context, dir string, n int) []Commit {
	out, err := run(ctx, dir, "log", "-"+strconv.Itoa(n), "--format=%h%x1f%s%x1f%an%x1f%ct%x00")
	if err != nil {
		return nil
	}
	var commits []Commit
	for rec := range strings.SplitSeq(string(out), "\x00") {
		f := strings.Split(strings.TrimLeft(rec, "\r\n"), "\x1f")
		if len(f) != 4 {
			continue
		}
		secs, _ := strconv.ParseInt(strings.TrimSpace(f[3]), 10, 64)
		commits = append(commits, Commit{Hash: f[0], Subject: f[1], Author: f[2], When: time.Unix(secs, 0)})
	}
	return commits
}
