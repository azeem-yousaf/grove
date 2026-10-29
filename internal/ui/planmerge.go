package ui

import (
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// mergedInto finds the plan whose finished tasks are merged in the worktree
// at path.
func (m *Model) mergedInto(path string) *plan.Plan {
	if path == "" {
		return nil
	}
	for _, p := range m.plans {
		if p.Merge != nil && p.Merge.Path != "" && git.SamePath(p.Merge.Path, path) {
			return p
		}
	}
	return nil
}

// latestStarted is the newest plan that has started and isn't done, else
// the newest started one.
func (m *Model) latestStarted() *plan.Plan {
	var done *plan.Plan
	for i := len(m.plans) - 1; i >= 0; i-- {
		switch p := m.plans[i]; p.Status {
		case plan.Draft:
		case plan.Done:
			if done == nil {
				done = p
			}
		default:
			return p
		}
	}
	return done
}
