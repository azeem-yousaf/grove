package git

import (
	"context"
	"fmt"
	"strings"
)

// DetectBase picks the branch that worktrees are compared against. An explicit
// override wins; otherwise it follows origin/HEAD, then falls back to the
// usual default branch names. Local branches are preferred over their remote
// counterparts. It returns "" when nothing suitable exists.
func DetectBase(ctx context.Context, r *Repo, override string) (string, error) {
	dir := r.Here
	if dir == "" {
		dir = r.CommonDir
	}
	if override != "" {
		if !refExists(ctx, dir, override) {
			return "", fmt.Errorf("base branch %q does not exist", override)
		}
		return override, nil
	}

	var candidates []string
	if out, err := run(ctx, dir, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil {
		remote := strings.TrimSpace(string(out)) // e.g. origin/main
		candidates = append(candidates, strings.TrimPrefix(remote, "origin/"), remote)
	}
	for _, name := range []string{"main", "master", "trunk", "develop"} {
		candidates = append(candidates, name, "origin/"+name)
	}
	for _, c := range candidates {
		if refExists(ctx, dir, c) {
			return c, nil
		}
	}
	return "", nil
}

func refExists(ctx context.Context, dir, ref string) bool {
	_, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}
