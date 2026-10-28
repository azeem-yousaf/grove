package git

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Branches is the repository's local and remote-tracking branch names.
type Branches struct {
	Local  map[string]bool // "feature/x"
	Remote map[string]bool // "origin/feature/x"
}

// ListBranches reads every local and remote-tracking branch.
func ListBranches(ctx context.Context, r *Repo) (Branches, error) {
	b := Branches{Local: map[string]bool{}, Remote: map[string]bool{}}
	out, err := run(ctx, r.CommonDir, "for-each-ref", "--format=%(refname)", "refs/heads", "refs/remotes")
	if err != nil {
		return b, err
	}
	for _, ref := range splitLines(out) {
		switch {
		case strings.HasPrefix(ref, "refs/heads/"):
			b.Local[strings.TrimPrefix(ref, "refs/heads/")] = true
		case strings.HasPrefix(ref, "refs/remotes/") && !strings.HasSuffix(ref, "/HEAD"):
			b.Remote[strings.TrimPrefix(ref, "refs/remotes/")] = true
		}
	}
	return b, nil
}

// BranchKind says where a branch for a new worktree comes from.
type BranchKind int

const (
	BranchNew    BranchKind = iota // created from a start point
	BranchLocal                    // an existing local branch, checked out as is
	BranchRemote                   // a new local branch tracking a remote one
)

// Resolve works out which branch a name typed by the user refers to. It
// returns the local branch name the worktree will have and, for remote
// branches, the remote-tracking ref to start from. "origin/x" and "x" both
// find origin/x when there is no local x.
func (b Branches) Resolve(name string) (local string, kind BranchKind, remote string) {
	if b.Local[name] {
		return name, BranchLocal, ""
	}
	if b.Remote[name] {
		if _, rest, ok := strings.Cut(name, "/"); ok {
			if b.Local[rest] {
				return rest, BranchLocal, ""
			}
			return rest, BranchRemote, name
		}
	}
	if b.Remote["origin/"+name] {
		return name, BranchRemote, "origin/" + name
	}
	for ref := range b.Remote {
		if _, rest, ok := strings.Cut(ref, "/"); ok && rest == name {
			return name, BranchRemote, ref
		}
	}
	return name, BranchNew, ""
}

// AddOptions describe a worktree to create.
type AddOptions struct {
	Path   string // folder for the new worktree; must not exist
	Branch string // as typed: new, existing local, or remote
	From   string // start point for a new branch; empty means HEAD
}

// AddWorktree creates a worktree and returns the local branch it checked
// out. A new branch doesn't track its start point, so a feature branched
// from origin/main won't pull main or push to it.
func AddWorktree(ctx context.Context, r *Repo, b Branches, opt AddOptions) (string, error) {
	if _, err := os.Stat(opt.Path); err == nil {
		return "", errors.New(opt.Path + " already exists")
	}
	local, kind, remote := b.Resolve(opt.Branch)
	var args []string
	switch kind {
	case BranchLocal:
		args = []string{"worktree", "add", opt.Path, local}
	case BranchRemote:
		args = []string{"worktree", "add", "--track", "-b", local, opt.Path, remote}
	default:
		if err := ValidBranchName(local); err != nil {
			return "", err
		}
		args = []string{"worktree", "add", "--no-track", "-b", local, opt.Path}
		if opt.From != "" {
			args = append(args, opt.From)
		}
	}
	if _, err := run(ctx, r.CommonDir, args...); err != nil {
		return "", err
	}
	return local, nil
}

// ValidBranchName applies git's branch naming rules (check-ref-format) so
// mistakes show while typing. git still has the final say when creating.
func ValidBranchName(name string) error {
	switch {
	case name == "":
		return errors.New("type a branch name")
	case name == "@" || name == "HEAD":
		return errors.New(name + " isn't a usable branch name")
	case strings.HasPrefix(name, "-"):
		return errors.New("can't start with -")
	case strings.ContainsAny(name, " ~^:?*[\\\t"):
		return errors.New(`can't contain spaces or ~ ^ : ? * [ \`)
	case strings.Contains(name, ".."), strings.Contains(name, "@{"), strings.Contains(name, "//"):
		return errors.New(`can't contain "..", "@{" or "//"`)
	case strings.HasPrefix(name, "/"), strings.HasSuffix(name, "/"), strings.HasSuffix(name, "."), strings.HasSuffix(name, ".lock"):
		return errors.New(`can't start or end with "/", or end with "." or ".lock"`)
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return errors.New(`no part of the name can start with "."`)
		}
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return errors.New("can't contain control characters")
		}
	}
	return nil
}

// DefaultWorktreePath suggests a folder next to the main worktree named
// after the repository and branch: C:\src\app + feature/x = C:\src\app-feature-x.
func DefaultWorktreePath(mainPath, repoName, branch string) string {
	slug := strings.Map(func(c rune) rune {
		switch c {
		case '/', '\\', ' ', ':', '*', '?', '"', '<', '>', '|':
			return '-'
		}
		return c
	}, strings.Trim(branch, "/"))
	if slug == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(mainPath), repoName+"-"+slug)
}

// MatchCopy returns the paths, relative to from, matched by the copy
// patterns. These are typically ignored files such as .env that a new
// worktree doesn't get from git.
func MatchCopy(from string, patterns []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range patterns {
		matches, _ := filepath.Glob(filepath.Join(from, filepath.FromSlash(p)))
		for _, m := range matches {
			rel, err := filepath.Rel(from, m)
			if err != nil || seen[rel] || strings.HasPrefix(rel, "..") {
				continue
			}
			seen[rel] = true
			out = append(out, rel)
		}
	}
	return out
}

// CopyInto copies files and folders (relative to from) into to, skipping
// anything that already exists there. It returns how many it copied.
func CopyInto(from, to string, rels []string) (int, error) {
	n := 0
	for _, rel := range rels {
		dst := filepath.Join(to, rel)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if err := copyPath(filepath.Join(from, rel), dst); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func copyPath(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(src, p)
			target := filepath.Join(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			return copyFile(p, target)
		})
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
