package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/git"
)

// conflictReview is a read-only preview until the user presses m.
type conflictReview struct {
	target int
	checks []mergeCheck
	cursor int
	busy   bool
}

type mergeCheck struct {
	source int
	paths  []string
	err    error
}

type conflictCheckMsg struct {
	target int
	checks []mergeCheck
}

func (m *Model) openConflictReview() tea.Cmd {
	if m.snap == nil || m.scanning {
		return nil
	}
	target := m.hereIdx
	if r, ok := m.selected(); ok {
		target = r.wt
	}
	if target < 0 || target >= len(m.snap.States) {
		return m.notify("Select a worktree to receive the other branches", toastInfo)
	}
	st := m.snap.States[target]
	if st.Bare || st.Prunable || st.Err != nil || st.Branch == "" || st.Head == "" {
		return m.notify("The target must be an active worktree on a branch", toastInfo)
	}
	m.conflicts = &conflictReview{target: target, busy: true}
	states := append([]git.State(nil), m.snap.States...)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
		defer cancel()
		var checks []mergeCheck
		for i, source := range states {
			if i == target || source.Bare || source.Prunable || source.Err != nil || source.Branch == "" || source.Head == "" {
				continue
			}
			paths, err := git.MergeConflicts(ctx, st.Path, st.Head, source.Head)
			checks = append(checks, mergeCheck{source: i, paths: paths, err: err})
		}
		return conflictCheckMsg{target: target, checks: checks}
	}
}

func (m *Model) conflictKey(msg tea.KeyPressMsg) tea.Cmd {
	c := m.conflicts
	if c == nil {
		return nil
	}
	switch msg.String() {
	case "esc", "q":
		if !c.busy {
			m.conflicts = nil
		}
	case "up", "k":
		c.cursor = max(0, c.cursor-1)
	case "down", "j":
		c.cursor = min(max(0, len(c.checks)-1), c.cursor+1)
	case "m":
		if !c.busy {
			return m.applyMergeReview()
		}
	case "ctrl+c":
		return m.quitNow()
	}
	return nil
}

func (m *Model) applyMergeReview() tea.Cmd {
	c := m.conflicts
	if c == nil || c.busy || len(c.checks) == 0 {
		return nil
	}
	target := m.snap.States[c.target]
	checks := append([]mergeCheck(nil), c.checks...)
	states := append([]git.State(nil), m.snap.States...)
	if hasUncommitted(target) {
		return m.notify("Commit or move the target's uncommitted changes before merging", toastErr)
	}
	for _, check := range checks {
		if check.err != nil {
			return m.notify("A merge preview failed; refresh and review again before applying", toastErr)
		}
		if hasUncommitted(states[check.source]) {
			return m.notify("Commit or move uncommitted changes in every source worktree before merging", toastErr)
		}
	}
	c.busy = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		defer cancel()
		for _, check := range checks {
			source := states[check.source]
			if err := git.Merge(ctx, target.Path, source.Head); err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "conflict") {
					return mergeDoneMsg{text: fmt.Sprintf("Merge stopped at %s with conflicts", source.Name()), err: err}
				}
				return mergeDoneMsg{text: fmt.Sprintf("Couldn't merge %s", source.Name()), err: err}
			}
		}
		return mergeDoneMsg{text: "Merged all active branches into " + target.Name()}
	}
}

func hasUncommitted(st git.State) bool {
	for _, f := range st.Files {
		if f.Uncommitted {
			return true
		}
	}
	return false
}

func (m *Model) openConflictAgent(target git.State) tea.Cmd {
	if target.Bare || target.Prunable || target.Err != nil {
		return m.notify("The target worktree is unavailable for conflict resolution", toastErr)
	}
	return m.withAgent(func(kind agent.Kind) tea.Cmd {
		s := m.sessionFor(target.Path)
		if s == nil {
			s = m.newSession(target.Path, target.Name(), kind)
		}
		prompt := "Resolve the merge conflicts currently present in this worktree. Inspect the conflicted files and git status, make the smallest correct changes, and do not commit or complete the merge. Explain the resolution and leave the merge ready for my review."
		return tea.Batch(m.showChat(s), m.send(s, prompt))
	})
}

func (m *Model) conflictModal() string {
	c, st := m.conflicts, m.st
	if c == nil {
		return ""
	}
	target := m.snap.States[c.target]
	lines := []string{st.muted.Render("Target: ") + st.bold.Render(target.Name()), ""}
	if c.busy {
		lines = append(lines, st.accent.Render("Checking merge results across active worktrees…"))
	} else if len(c.checks) == 0 {
		lines = append(lines, st.add.Render("No other active branches to merge."))
	} else {
		for i, check := range c.checks {
			source := m.snap.States[check.source]
			prefix := "  "
			if i == c.cursor {
				prefix = st.accent.Render("› ")
			}
			switch {
			case check.err != nil:
				lines = append(lines, prefix+st.bold.Render(source.Name())+st.del.Render(" · preview failed: "+check.err.Error()))
			case len(check.paths) == 0:
				lines = append(lines, prefix+st.bold.Render(source.Name())+st.add.Render(" · clean merge"))
			default:
				lines = append(lines, prefix+st.bold.Render(source.Name())+st.warn.Render(" · "+plural(len(check.paths), "conflict")))
				for _, p := range check.paths {
					lines = append(lines, "    "+st.text.Render(p))
				}
			}
		}
	}
	lines = append(lines, "", st.faint.Render("Preview does not change files. m merges each branch in order; git will stop on a conflict."))
	foot := m.hints([][2]string{{"m", "apply merges"}, {"esc", "cancel"}})
	return st.modal.Padding(1, 2).Width(min(m.width-8, 76)).Render(st.bold.Render("Review worktree merges") + "\n\n" + strings.Join(lines, "\n") + "\n\n" + foot)
}
