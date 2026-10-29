package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Hunk is one hunk of a unified diff, exactly as git wrote it.
type Hunk struct {
	Path               string // repo-relative path on the new side, forward slashes
	OldStart, OldCount int
	NewStart, NewCount int
	Old, New           []string // the old side's lines (context and removed), and the new side's
	OldNoEOL, NewNoEOL bool     // that side's last line has no newline
	NewFile            bool     // the old side is /dev/null
	DeletedFile        bool     // the new side is /dev/null
	Lines              []string // the hunk as written, with its +, - and space marks
}

// ParseHunks reads every hunk of a unified diff, for one file or many.
func ParseHunks(raw string) []Hunk {
	var (
		hunks            []Hunk
		path             string
		newFile, deleted bool
		cur              *Hunk
		last             byte // mark of the previous hunk line
		inHeader         = true
		oldLeft, newLeft int
		flush            = func() { cur = nil }
		minusPath        string
	)
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimSuffix(l, "\r")
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flush()
			inHeader, newFile, deleted, path, minusPath = true, false, false, diffGitPath(l), ""
			continue
		case inHeader && strings.HasPrefix(l, "--- "):
			minusPath = strings.TrimSuffix(strings.TrimPrefix(l, "--- "), "\t")
			newFile = minusPath == "/dev/null"
			continue
		case inHeader && strings.HasPrefix(l, "+++ "):
			// A name with spaces ends in a tab, the way GNU diff writes it.
			plusTo := strings.TrimSuffix(strings.TrimPrefix(l, "+++ "), "\t")
			deleted = plusTo == "/dev/null"
			if !deleted {
				path = strings.TrimPrefix(plusTo, "b/")
			} else if strings.HasPrefix(minusPath, "a/") {
				path = strings.TrimPrefix(minusPath, "a/")
			}
			continue
		case strings.HasPrefix(l, "@@"):
			inHeader = false
			oldStart, oc, newStart, nc, ok := parseRanges(l)
			if !ok {
				flush()
				continue
			}
			hunks = append(hunks, Hunk{Path: path, OldStart: oldStart, OldCount: oc, NewStart: newStart, NewCount: nc, NewFile: newFile, DeletedFile: deleted})
			cur = &hunks[len(hunks)-1]
			oldLeft, newLeft, last = oc, nc, 0
			continue
		}
		if cur == nil || inHeader {
			continue
		}
		switch {
		case strings.HasPrefix(l, `\`):
			switch last {
			case '-':
				cur.OldNoEOL = true
			case '+':
				cur.NewNoEOL = true
			case ' ':
				cur.OldNoEOL, cur.NewNoEOL = true, true
			}
			cur.Lines = append(cur.Lines, l)
			continue
		case oldLeft == 0 && newLeft == 0:
			flush()
			continue
		case strings.HasPrefix(l, "-"):
			cur.Old = append(cur.Old, l[1:])
			oldLeft--
		case strings.HasPrefix(l, "+"):
			cur.New = append(cur.New, l[1:])
			newLeft--
		case strings.HasPrefix(l, " "), l == "":
			text := ""
			if l != "" {
				text = l[1:]
			}
			cur.Old, cur.New = append(cur.Old, text), append(cur.New, text)
			oldLeft--
			newLeft--
			l = " " + text
		default:
			flush()
			continue
		}
		last = l[0]
		cur.Lines = append(cur.Lines, l)
	}
	return hunks
}

// diffGitPath is the new path of a "diff --git a/x b/y" line.
func diffGitPath(l string) string {
	rest := strings.TrimPrefix(l, "diff --git ")
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return ""
}

// parseRanges reads "@@ -12,7 +12,9 @@": a missing count is 1.
func parseRanges(l string) (oldStart, oldCount, newStart, newCount int, ok bool) {
	parts := strings.SplitN(l, "@@", 3)
	if len(parts) < 3 {
		return 0, 0, 0, 0, false
	}
	var seen int
	for f := range strings.FieldsSeq(parts[1]) {
		start, count, hasCount := strings.Cut(f[1:], ",")
		s, err := strconv.Atoi(start)
		if err != nil {
			return 0, 0, 0, 0, false
		}
		c := 1
		if hasCount {
			if c, err = strconv.Atoi(count); err != nil {
				return 0, 0, 0, 0, false
			}
		}
		switch f[0] {
		case '-':
			oldStart, oldCount = s, c
			seen++
		case '+':
			newStart, newCount = s, c
			seen++
		}
	}
	return oldStart, oldCount, newStart, newCount, seen == 2
}

// ErrHunkMoved means the file no longer has the hunk's new side where the
// diff said, usually because it changed since.
var ErrHunkMoved = errors.New("the file changed since this diff was shown; look again")

// RevertHunk puts the old side of h back in its file under dir, the way
// git apply -R would. It edits the file itself, keeping its line endings,
// so it works on CRLF files that a patch with LF lines wouldn't apply to.
// Reverting a new file's only hunk deletes the file, and a deleted file's
// brings it back.
func RevertHunk(dir string, h Hunk) error {
	path := filepath.Join(dir, filepath.FromSlash(h.Path))
	if h.NewFile && h.OldCount == 0 {
		if err := checkLines(path, h); err != nil {
			return err
		}
		return os.Remove(path)
	}
	data, err := os.ReadFile(path)
	mode := os.FileMode(0o644)
	switch {
	case err == nil:
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	case errors.Is(err, os.ErrNotExist) && (h.DeletedFile || h.NewCount == 0 && h.NewStart == 0):
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	default:
		return err
	}
	lines, eol, endsNL := splitFile(string(data))

	at := h.NewStart - 1
	if h.NewCount == 0 {
		at = h.NewStart // the side is empty: it starts after this line
	}
	if at < 0 || at+h.NewCount > len(lines) || !sameLines(lines[at:at+h.NewCount], h.New) {
		return ErrHunkMoved
	}
	atEnd := at+h.NewCount == len(lines)
	out := append(append(append([]string{}, lines[:at]...), h.Old...), lines[at+h.NewCount:]...)
	if atEnd {
		endsNL = !h.OldNoEOL
	}
	text := strings.Join(out, eol)
	if endsNL && len(out) > 0 {
		text += eol
	}
	return os.WriteFile(path, []byte(text), mode)
}

// checkLines makes sure path still holds exactly h's new side.
func checkLines(path string, h Hunk) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines, _, _ := splitFile(string(data))
	if !sameLines(lines, h.New) {
		return ErrHunkMoved
	}
	return nil
}

// splitFile splits text into lines without their endings, reporting the
// line ending it uses and whether the last line has one.
func splitFile(text string) (lines []string, eol string, endsNL bool) {
	eol = "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	if text == "" {
		return nil, eol, false
	}
	endsNL = strings.HasSuffix(text, "\n")
	lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines, eol, endsNL
}

// sameLines compares lines, ignoring a carriage return at their ends: the
// diff may come from git's normalised view of a CRLF file.
func sameLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.TrimSuffix(a[i], "\r") != strings.TrimSuffix(b[i], "\r") {
			return false
		}
	}
	return true
}

// WorktreeDiff is the diff of every changed file of st, in the order of
// st.Files: one git diff for tracked files and one per untracked file.
func WorktreeDiff(ctx context.Context, st State) (string, error) {
	out, err := run(ctx, st.Path, "diff", "--no-color", "--no-ext-diff", "-M", st.Rev)
	if err != nil {
		return "", err
	}
	sections := map[string]string{}
	var unknown []string
	for _, sec := range splitSections(string(out)) {
		first, _, _ := strings.Cut(sec, "\n")
		if p := diffGitPath(first); p != "" {
			sections[p] = sec
		} else {
			unknown = append(unknown, sec)
		}
	}
	var b strings.Builder
	for _, f := range st.Files {
		if f.Status == Untracked {
			d, err := Diff(ctx, st, f)
			if err != nil {
				return "", fmt.Errorf("%s: %w", f.Path, err)
			}
			b.WriteString(d)
			continue
		}
		if sec, ok := sections[f.Path]; ok {
			b.WriteString(sec)
			delete(sections, f.Path)
		}
	}
	// Anything git listed that the scan didn't, for example a file changed
	// since: still show it.
	for _, sec := range sections {
		unknown = append(unknown, sec)
	}
	for _, sec := range unknown {
		b.WriteString(sec)
	}
	return b.String(), nil
}

// splitSections splits a multi-file diff at each "diff --git" line.
func splitSections(raw string) []string {
	var out []string
	start := -1
	for i := 0; i < len(raw); {
		if strings.HasPrefix(raw[i:], "diff --git ") {
			if start >= 0 {
				out = append(out, raw[start:i])
			}
			start = i
		}
		next := strings.IndexByte(raw[i:], '\n')
		if next < 0 {
			break
		}
		i += next + 1
	}
	if start >= 0 {
		out = append(out, raw[start:])
	}
	return out
}

// DiffSince is the diff from the checkpoint tree to the worktree at dir as
// it is now, untracked files included. Both sides are exact bytes.
func DiffSince(ctx context.Context, dir, tree string) (string, error) {
	now, err := Checkpoint(ctx, dir)
	if err != nil {
		return "", err
	}
	out, err := run(ctx, dir, "diff", "--no-color", "--no-ext-diff", "-M", tree, now)
	return string(out), err
}
