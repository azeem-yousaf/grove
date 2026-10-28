package git

import (
	"context"
	"strings"
)

// Worktree is one entry from `git worktree list`.
type Worktree struct {
	Path     string
	Head     string
	Branch   string // short branch name; empty when detached
	Main     bool   // the repository's main worktree
	Bare     bool
	Detached bool
	Locked   bool
	Prunable bool // git knows about it but its folder is gone

	LockReason  string
	PruneReason string
}

// Name is the label shown for the worktree: its branch, or the short commit
// it is detached at.
func (w Worktree) Name() string {
	switch {
	case w.Branch != "":
		return w.Branch
	case w.Bare:
		return "(bare)"
	case len(w.Head) >= 7:
		return "detached @ " + w.Head[:7]
	default:
		return "detached"
	}
}

// Worktrees lists every worktree of the repository, main worktree first.
func Worktrees(ctx context.Context, r *Repo) ([]Worktree, error) {
	dir := r.Here
	if dir == "" {
		dir = r.CommonDir
	}
	out, err := run(ctx, dir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(out), nil
}

// parseWorktrees parses `git worktree list --porcelain -z`. Each attribute is
// NUL-terminated and records are separated by an empty attribute.
func parseWorktrees(b []byte) []Worktree {
	var (
		list []Worktree
		cur  *Worktree
	)
	flush := func() {
		if cur != nil {
			list = append(list, *cur)
			cur = nil
		}
	}
	for _, field := range strings.Split(string(b), "\x00") {
		if field == "" {
			flush()
			continue
		}
		key, val, _ := strings.Cut(field, " ")
		if key == "worktree" {
			flush()
			cur = &Worktree{Path: nativePath(val)}
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "HEAD":
			cur.Head = val
		case "branch":
			cur.Branch = strings.TrimPrefix(val, "refs/heads/")
		case "detached":
			cur.Detached = true
		case "bare":
			cur.Bare = true
		case "locked":
			cur.Locked = true
			cur.LockReason = val
		case "prunable":
			cur.Prunable = true
			cur.PruneReason = val
		}
	}
	flush()
	if len(list) > 0 {
		list[0].Main = true
	}
	return list
}
