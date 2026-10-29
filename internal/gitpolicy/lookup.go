package gitpolicy

import (
	"context"
	"sync"

	"github.com/azeem-yousaf/grove/internal/git"
)

// GitLookup answers Lookup's questions with git in dir. Answers are cached, so
// make one per Classify call to see the repository as it is now.
func GitLookup(ctx context.Context, dir string) Lookup {
	return &gitLookup{ctx: ctx, dir: dir,
		branch: map[string]branchAnswer{}, alias: map[string]aliasAnswer{},
		refs: map[string]bool{}, pushed: map[string]bool{}}
}

type branchAnswer struct {
	name string
	err  error
}

type aliasAnswer struct {
	value string
	ok    bool
}

type gitLookup struct {
	ctx    context.Context
	dir    string
	mu     sync.Mutex
	branch map[string]branchAnswer
	alias  map[string]aliasAnswer
	refs   map[string]bool
	pushed map[string]bool
}

func (g *gitLookup) CurrentBranch(dir string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a, ok := g.branch[dir]; ok {
		return a.name, a.err
	}
	name, err := git.CurrentBranch(g.ctx, dir)
	g.branch[dir] = branchAnswer{name, err}
	return name, err
}

func (g *gitLookup) Alias(name string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a, ok := g.alias[name]; ok {
		return a.value, a.ok
	}
	v, ok := git.AliasOf(g.ctx, g.dir, name)
	g.alias[name] = aliasAnswer{v, ok}
	return v, ok
}

func (g *gitLookup) RefExists(ref string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if v, ok := g.refs[ref]; ok {
		return v
	}
	v := git.RefExists(g.ctx, g.dir, ref)
	g.refs[ref] = v
	return v
}

// Pushed counts a failed check as pushed, so rewriting history then asks.
func (g *gitLookup) Pushed(branch string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if v, ok := g.pushed[branch]; ok {
		return v
	}
	v, err := git.Pushed(g.ctx, g.dir, branch)
	v = v || err != nil
	g.pushed[branch] = v
	return v
}
