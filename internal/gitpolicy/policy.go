// Package gitpolicy decides, without any AI, whether a shell command an agent
// wants to run may run on its own. It parses the command line the way Bash or
// PowerShell would, finds every git invocation in it, and classifies each one
// against the agent's Scope: routine work on its own non-protected branch is
// allowed, anything touching a protected branch, anything destructive and
// anything it cannot understand needs the user.
package gitpolicy

import (
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// Shell is the shell a command is written for.
type Shell int

const (
	Bash Shell = iota
	PowerShell
)

func (s Shell) String() string {
	if s == PowerShell {
		return "PowerShell"
	}
	return "Bash"
}

// Scope is what an agent may touch.
type Scope struct {
	Worktree  string   // the agent's own worktree folder
	Branch    string   // its checked-out branch, "" if detached (informational: targets come from Lookup)
	Dir       string   // the folder the command starts in; "" means Worktree
	Readable  []string // other folders it may read (groupmates, main worktree)
	Writable  []string // folders besides Worktree it may change (the planner: its plan's task and merge worktrees)
	Protected []string // branch names or globs: main, master, the default branch, config [git] protected
	Guarded   bool     // the git-level guard is verified for this worktree
}

// Verdict is what should happen to a command.
type Verdict int

const (
	NotGit Verdict = iota // no git involved: leave it to the normal approval flow
	Allow                 // routine: may run without asking
	User                  // needs the user's explicit approval
)

func (v Verdict) String() string {
	switch v {
	case Allow:
		return "allow"
	case User:
		return "user"
	}
	return "not git"
}

// Decision is Classify's answer.
type Decision struct {
	Verdict     Verdict
	Reason      string   // short and user-facing, e.g. "merges into protected branch master"
	Protected   bool     // it would change a protected branch
	Destructive bool     // it could lose work: discarded changes, deleted branches, force pushes
	Bypass      bool     // it could get around grove's git guard: hooks skipped or replaced, git's config or .git changed directly, an unguarded worktree
	Refs        []string // branches it would change
}

// Lookup answers the questions Classify needs from the repository.
type Lookup interface {
	CurrentBranch(dir string) (string, error) // "" when HEAD is detached
	Alias(name string) (string, bool)
	RefExists(ref string) bool
	Pushed(branch string) bool // the branch has been pushed: a remote branch of the same name exists
}

// Classify decides whether cmd, written for sh, may run on its own within sc.
// It never runs git itself; everything it needs from the repository comes
// from lk, and only when the command makes it necessary.
func Classify(cmd string, sh Shell, sc Scope, lk Lookup) Decision {
	c := &classifier{sc: sc, lk: lk, sh: sh}
	return c.run(cmd)
}

// shortBranch strips refs/heads/ or heads/ from a branch name.
func shortBranch(ref string) string {
	if s, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return s
	}
	if s, ok := strings.CutPrefix(ref, "heads/"); ok {
		return s
	}
	return ref
}

// isProtected reports whether branch matches one of the protected names or
// globs. Case is ignored: on case-insensitive file systems Master and master
// are the same loose ref.
func isProtected(branch string, protected []string) bool {
	b := strings.ToLower(shortBranch(branch))
	if b == "" {
		return false
	}
	for _, p := range protected {
		p = strings.ToLower(shortBranch(strings.TrimSpace(p)))
		if p == "" {
			continue
		}
		if p == b {
			return true
		}
		if ok, err := path.Match(p, b); err == nil && ok {
			return true
		}
	}
	return false
}

// inside reports whether p is root or inside it.
func inside(p, root string) bool {
	if p == "" || root == "" {
		return false
	}
	p, root = filepath.Clean(p), filepath.Clean(root)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		p, root = strings.ToLower(p), strings.ToLower(root)
	}
	if p == root {
		return true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func insideAny(p string, roots []string) bool {
	for _, r := range roots {
		if inside(p, r) {
			return true
		}
	}
	return false
}
