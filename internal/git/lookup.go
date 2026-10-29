package git

import (
	"context"
	"strings"
)

// CurrentBranch returns the short name of the branch checked out in dir, or ""
// when HEAD is detached (which includes the middle of a rebase).
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := runCodes(ctx, dir, []int{1}, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// AliasOf returns what `git <name>` expands to when name is a git alias.
func AliasOf(ctx context.Context, dir, name string) (string, bool) {
	out, err := runCodes(ctx, dir, []int{1}, "config", "--get", "alias."+name)
	if err != nil {
		return "", false
	}
	v := strings.TrimSpace(string(out))
	return v, v != ""
}

// RefExists reports whether ref names a commit.
func RefExists(ctx context.Context, dir, ref string) bool { return refExists(ctx, dir, ref) }

// Pushed reports whether branch has been pushed: some remote has a branch of
// the same name. Rewriting its commits would then need a force push.
func Pushed(ctx context.Context, dir, branch string) (bool, error) {
	out, err := run(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/remotes/*/"+branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}
