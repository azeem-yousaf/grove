package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/charmbracelet/x/ansi"
)

// mergeStage is where the merge review is. Nothing changes in a worktree
// until the user confirms the planned merges.
type mergeStage uint8

const (
	stageChecking mergeStage = iota // previewing each branch against the target
	stageChoose                     // picking the branches to merge
	stagePlanning                   // previewing the chosen merges in order
	stageConfirm                    // showing that plan before merging
	stageMerging                    // git is merging
	stageConflict                   // a merge stopped with conflicts in the target
	stageAborting                   // git merge --abort is running
)

// conflictReview merges other worktrees' branches into one worktree. It
// keeps worktrees by path and branch, never by snapshot index, because
// scans carry on while it is open.
type conflictReview struct {
	stage  mergeStage
	seq    int    // drops results for a review that was closed or rechecked
	path   string // target worktree
	name   string
	branch string
	checks []mergeCheck
	cursor int
	plan   []git.MergeStep

	merged     []string // branches merged before a conflict stopped the run
	stopped    string   // the branch whose merge stopped
	conflicted []string // unmerged files in the target
	remaining  []string // chosen branches not merged yet
}

// mergeCheck is one other worktree's branch, previewed against the target.
type mergeCheck struct {
	name     string
	branch   string
	commits  int      // commits the target would gain
	paths    []string // files that would conflict
	dirty    bool     // uncommitted work the merge won't include
	err      error
	selected bool
}

// busy reports whether git is working for the review.
func (c *conflictReview) busy() bool {
	switch c.stage {
	case stageChecking, stagePlanning, stageMerging, stageAborting:
		return true
	}
	return false
}

func (c mergeCheck) mergeable() bool { return c.err == nil && c.commits > 0 }

type (
	conflictCheckMsg struct {
		seq        int
		checks     []mergeCheck
		inMerge    bool // the target is already in the middle of a merge
		conflicted []string
		err        error
	}
	mergePlanMsg struct {
		seq  int
		plan []git.MergeStep
	}
	mergeDoneMsg struct {
		seq        int
		merged     []string
		stopped    string
		conflicted []string
		remaining  []string
		err        error
	}
	mergeAbortMsg struct {
		seq int
		err error
	}
)

func (m *Model) openConflictReview() tea.Cmd {
	if m.snap == nil {
		return nil
	}
	target := m.hereIdx
	if r, ok := m.selected(); ok {
		target = r.wt
	}
	if target < 0 || target >= len(m.snap.States) {
		return m.notify("Select the worktree to merge other branches into", toastInfo)
	}
	st := m.snap.States[target]
	if st.Bare || st.Prunable || st.Err != nil || st.Branch == "" {
		return m.notify("Merges need a worktree that has a branch checked out", toastInfo)
	}
	m.mergeSeq++
	c := &conflictReview{stage: stageChecking, seq: m.mergeSeq, path: st.Path, name: st.Name(), branch: st.Branch}
	m.conflicts = c
	type source struct {
		name, branch string
		dirty        bool
	}
	var sources []source
	for i, s := range m.snap.States {
		if i == target || s.Bare || s.Prunable || s.Err != nil || s.Branch == "" {
			continue
		}
		sources = append(sources, source{s.Name(), s.Branch, hasUncommitted(s)})
	}
	seq, dir, into := c.seq, c.path, c.branch
	return tea.Batch(m.startAnimating(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
		defer cancel()
		if busy, err := git.MergeInProgress(ctx, dir); err != nil || busy {
			paths, uerr := git.UnmergedPaths(ctx, dir)
			if err == nil {
				err = uerr
			}
			return conflictCheckMsg{seq: seq, inMerge: busy, conflicted: paths, err: err}
		}
		checks := make([]mergeCheck, 0, len(sources))
		for _, s := range sources {
			c := mergeCheck{name: s.name, branch: s.branch, dirty: s.dirty}
			c.commits, c.err = git.CommitsBetween(ctx, dir, into, s.branch)
			if c.err == nil && c.commits > 0 {
				_, c.paths, c.err = git.MergeConflicts(ctx, dir, into, s.branch)
			}
			checks = append(checks, c)
		}
		return conflictCheckMsg{seq: seq, checks: checks}
	})
}

func (m *Model) onConflictCheck(msg conflictCheckMsg) tea.Cmd {
	c := m.conflicts
	if c == nil || c.seq != msg.seq {
		return nil
	}
	switch {
	case msg.inMerge:
		c.stage, c.conflicted = stageConflict, msg.conflicted
	case msg.err != nil:
		m.conflicts = nil
		return m.notify("Couldn't check "+c.name+": "+msg.err.Error(), toastErr)
	default:
		c.stage, c.checks, c.cursor = stageChoose, msg.checks, 0
	}
	return nil
}

func (m *Model) conflictKey(msg tea.KeyPressMsg) tea.Cmd {
	c := m.conflicts
	key := msg.String()
	if key == "ctrl+c" {
		return m.quitNow()
	}
	switch c.stage {
	case stageChecking, stagePlanning:
		if key == "esc" || key == "q" {
			m.conflicts = nil
		}
	case stageChoose:
		return m.onChooseKey(key)
	case stageConfirm:
		switch key {
		case "esc", "backspace":
			c.stage = stageChoose
		case "enter":
			return m.applyMerges()
		}
	case stageConflict:
		return m.onMergeConflictKey(key)
	}
	return nil
}

func (m *Model) onChooseKey(key string) tea.Cmd {
	c := m.conflicts
	switch key {
	case "esc", "q":
		m.conflicts = nil
	case "up", "k":
		c.cursor = max(0, c.cursor-1)
	case "down", "j":
		c.cursor = min(max(0, len(c.checks)-1), c.cursor+1)
	case "space":
		if c.cursor < len(c.checks) && c.checks[c.cursor].mergeable() {
			c.checks[c.cursor].selected = !c.checks[c.cursor].selected
		}
	case "a":
		// Select every mergeable branch, or clear them all if they already are.
		all := true
		for _, ch := range c.checks {
			if ch.mergeable() && !ch.selected {
				all = false
			}
		}
		for i := range c.checks {
			c.checks[i].selected = !all && c.checks[i].mergeable()
		}
	case "r":
		return m.recheckMerges()
	case "enter":
		return m.planMerges()
	}
	return nil
}

func (m *Model) recheckMerges() tea.Cmd {
	c := m.conflicts
	i := m.snapIndex(c.path)
	if i < 0 {
		m.conflicts = nil
		return m.notify(c.name+" is no longer a worktree", toastErr)
	}
	m.lastSel = m.snap.States[i].Path
	m.rebuildRows()
	return m.openConflictReview()
}

func (m *Model) chosenBranches() []string {
	var out []string
	for _, ch := range m.conflicts.checks {
		if ch.selected {
			out = append(out, ch.branch)
		}
	}
	return out
}

// planMerges previews the chosen merges one after another, because two
// branches that each merge cleanly can still conflict with each other.
func (m *Model) planMerges() tea.Cmd {
	c := m.conflicts
	branches := m.chosenBranches()
	if len(branches) == 0 {
		return m.notify("Choose branches to merge with space, or a for all", toastInfo)
	}
	c.stage = stagePlanning
	seq, dir, into := c.seq, c.path, c.branch
	return tea.Batch(m.startAnimating(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
		defer cancel()
		return mergePlanMsg{seq, git.PreviewMerges(ctx, dir, into, branches)}
	})
}

func (m *Model) onMergePlan(msg mergePlanMsg) tea.Cmd {
	if c := m.conflicts; c != nil && c.seq == msg.seq && c.stage == stagePlanning {
		c.plan, c.stage = msg.plan, stageConfirm
	}
	return nil
}

func (m *Model) planFailed() error {
	for _, s := range m.conflicts.plan {
		if s.Err != nil {
			return s.Err
		}
	}
	return nil
}

func (m *Model) applyMerges() tea.Cmd {
	c := m.conflicts
	if err := m.planFailed(); err != nil {
		return m.notify("The preview failed, so nothing was merged: "+err.Error(), toastErr)
	}
	branches := m.chosenBranches()
	c.stage = stageMerging
	seq, dir, name := c.seq, c.path, c.name
	return tea.Batch(m.startAnimating(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		defer cancel()
		// Checked now rather than from the last scan, which can be seconds old.
		dirty, err := git.HasTrackedChanges(ctx, dir)
		if err != nil {
			return mergeDoneMsg{seq: seq, err: err}
		}
		if dirty {
			return mergeDoneMsg{seq: seq, err: fmt.Errorf("%s has uncommitted changes; commit or stash them first", name)}
		}
		var merged []string
		for i, b := range branches {
			conflicted, err := git.Merge(ctx, dir, b)
			if err != nil {
				return mergeDoneMsg{seq: seq, merged: merged, err: err}
			}
			if len(conflicted) > 0 {
				return mergeDoneMsg{seq: seq, merged: merged, stopped: b, conflicted: conflicted, remaining: branches[i+1:]}
			}
			merged = append(merged, b)
		}
		return mergeDoneMsg{seq: seq, merged: merged}
	})
}

func (m *Model) onMergeDone(msg mergeDoneMsg) tea.Cmd {
	c := m.conflicts
	if c == nil || c.seq != msg.seq {
		return nil
	}
	done := ""
	if len(msg.merged) > 0 {
		done = "Merged " + strings.Join(msg.merged, ", ") + " into " + c.name
	}
	switch {
	case msg.err != nil:
		m.conflicts = nil
		text := "Couldn't merge: " + msg.err.Error()
		if done != "" {
			text = done + ". " + text
		}
		return tea.Batch(m.notify(text, toastErr), m.startScan())
	case msg.stopped != "":
		c.stage = stageConflict
		c.merged, c.stopped, c.conflicted, c.remaining = msg.merged, msg.stopped, msg.conflicted, msg.remaining
		return m.startScan()
	}
	m.conflicts = nil
	return tea.Batch(m.notify(done, toastOK), m.startScan())
}

func (m *Model) onMergeConflictKey(key string) tea.Cmd {
	c := m.conflicts
	switch key {
	case "esc", "q":
		m.conflicts = nil
		return m.notify("The merge is still in progress in "+c.name+"; press c there to come back to it", toastInfo)
	case "a":
		m.conflicts = nil
		return m.askAgentToResolve(c)
	case "e":
		opener, path, name := m.opener, c.path, c.name
		return action(func() (string, error) {
			via, err := opener.Folder(path)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("Opened %s in %s", name, via), nil
		})
	case "x":
		c.stage = stageAborting
		seq, dir := c.seq, c.path
		return tea.Batch(m.startAnimating(), func() tea.Msg {
			ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
			defer cancel()
			return mergeAbortMsg{seq, git.AbortMerge(ctx, dir)}
		})
	}
	return nil
}

func (m *Model) onMergeAbort(msg mergeAbortMsg) tea.Cmd {
	c := m.conflicts
	if c == nil || c.seq != msg.seq {
		return nil
	}
	m.conflicts = nil
	if msg.err != nil {
		return tea.Batch(m.notify("Couldn't abort the merge: "+msg.err.Error(), toastErr), m.startScan())
	}
	text := "Aborted the merge in " + c.name
	if len(c.merged) > 0 {
		text += "; " + strings.Join(c.merged, ", ") + " stayed merged"
	}
	return tea.Batch(m.notify(text, toastOK), m.startScan())
}

func hasUncommitted(st git.State) bool {
	for _, f := range st.Files {
		if f.Uncommitted {
			return true
		}
	}
	return false
}

// askAgentToResolve hands the conflicted merge to the target's agent, once
// the user has asked for it.
func (m *Model) askAgentToResolve(c *conflictReview) tea.Cmd {
	if s := m.sessionFor(c.path); s != nil && s.running {
		return tea.Batch(m.showChat(s), m.notify("The agent is busy; ask it once it finishes", toastInfo))
	}
	from := c.stopped
	if from == "" {
		from = "another branch"
	}
	prompt := fmt.Sprintf("A merge of %s into %s stopped with conflicts in: %s.\n\n"+
		"Resolve these conflicts. Read both sides of each conflict and the surrounding code, keep the intent of both branches, "+
		"and remove every conflict marker. Do not commit, do not run git merge --continue or --abort, and do not touch other files "+
		"unless the resolution needs it. When you are done, list each file and explain how you resolved it so I can review before committing.",
		from, c.branch, strings.Join(c.conflicted, ", "))
	path, name := c.path, c.name
	return m.withAgent(func(kind agent.Kind) tea.Cmd {
		s := m.sessionFor(path)
		if s == nil {
			s = m.newSession(path, name, kind)
		}
		return tea.Batch(m.showChat(s), m.send(s, prompt))
	})
}

// conflictModal draws the merge review for its current stage.
func (m *Model) conflictModal() string {
	c, st := m.conflicts, m.st
	width := min(m.width-6, 84)
	inner := width - 6 // border and padding
	room := max(m.height-14, 3)
	spin := st.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)]) + " "
	trunc := func(s string) string { return ansi.Truncate(s, inner, "…") }

	title := "Merge branches into " + c.name
	lines := []string{}
	var keys [][2]string
	switch c.stage {
	case stageChecking:
		lines = append(lines, spin+st.muted.Render("Checking what each branch would bring into "+c.name+"…"))
		keys = [][2]string{{"esc", "cancel"}}

	case stageChoose:
		if len(c.checks) == 0 {
			lines = append(lines, st.muted.Render("No other worktree has a branch to merge."))
			keys = [][2]string{{"esc", "close"}}
			break
		}
		lines = append(lines, st.muted.Render("Each branch is checked on its own against ")+st.text.Render(c.branch)+st.muted.Render(". Nothing changes yet."), "")
		start := max(0, min(c.cursor-room/2, len(c.checks)-room))
		for i := start; i < min(len(c.checks), start+room); i++ {
			lines = append(lines, trunc(m.checkLine(c.checks[i], i == c.cursor)))
		}
		if len(c.checks) > room {
			lines = append(lines, st.faint.Render(fmt.Sprintf("  %d of %d branches", min(len(c.checks), start+room)-start, len(c.checks))))
		}
		keys = [][2]string{{"space", "choose"}, {"a", "all"}, {"enter", "preview"}, {"r", "recheck"}, {"esc", "close"}}

	case stagePlanning:
		lines = append(lines, spin+st.muted.Render("Previewing the merges in order…"))
		keys = [][2]string{{"esc", "cancel"}}

	case stageConfirm:
		lines = append(lines, st.muted.Render("Merging into ")+st.text.Render(c.branch)+st.muted.Render(" in this order:"), "")
		stop := false
		var steps []string
		chosen := m.chosenBranches()
		for i, b := range chosen {
			n := fmt.Sprintf("%d. ", i+1)
			switch {
			case i > len(c.plan):
				continue
			case i == len(c.plan):
				// The run stops at the conflict, so these wait for another one.
				steps = append(steps, st.faint.Render(trunc(fmt.Sprintf("  %d more left for a later merge, once the conflict is committed", len(chosen)-i))))
			case c.plan[i].Err != nil:
				steps = append(steps, trunc("  "+n+st.bold.Render(b)+st.del.Render(" · preview failed: "+c.plan[i].Err.Error())))
			case len(c.plan[i].Conflicts) > 0:
				stop = true
				steps = append(steps, trunc("  "+n+st.bold.Render(b)+st.warn.Render(" · "+plural(len(c.plan[i].Conflicts), "conflict")+", the merge stops here")))
				for j, p := range c.plan[i].Conflicts {
					if j == 4 && len(c.plan[i].Conflicts) > 5 {
						steps = append(steps, st.faint.Render(fmt.Sprintf("       … %d more", len(c.plan[i].Conflicts)-4)))
						break
					}
					steps = append(steps, trunc("       "+st.text.Render(p)))
				}
			default:
				steps = append(steps, trunc("  "+n+st.bold.Render(b)+st.add.Render(" · clean")))
			}
		}
		if len(steps) > room {
			// Keep the end in view: it holds the conflict and what follows.
			steps = append([]string{st.faint.Render(fmt.Sprintf("  … %d clean merges before these", len(steps)-room+1))}, steps[len(steps)-room+1:]...)
		}
		lines = append(lines, steps...)
		lines = append(lines, "")
		switch {
		case m.planFailed() != nil:
			lines = append(lines, st.del.Render("Fix the failed preview before merging."))
			keys = [][2]string{{"esc", "back"}}
		case stop:
			lines = append(lines, st.muted.Render(trunc("git commits the clean merges, then stops at the conflict so you or the")),
				st.muted.Render(trunc("agent can resolve it. Nothing is committed for that branch until you do.")))
			keys = [][2]string{{"enter", "merge"}, {"esc", "back"}}
		default:
			lines = append(lines, st.muted.Render(trunc("Each branch becomes a merge commit on "+c.branch+".")))
			keys = [][2]string{{"enter", "merge"}, {"esc", "back"}}
		}

	case stageMerging:
		lines = append(lines, spin+st.muted.Render("Merging into "+c.name+"…"))

	case stageConflict, stageAborting:
		title = "Merge conflict in " + c.name
		from := "A merge"
		if c.stopped != "" {
			from = "Merging " + c.stopped
		}
		lines = append(lines, st.warn.Bold(true).Render(trunc("⚠ "+from+" stopped with conflicts in "+plural(len(c.conflicted), "file")+":")))
		for i, p := range c.conflicted {
			if i == room-1 && len(c.conflicted) > room {
				lines = append(lines, st.faint.Render(fmt.Sprintf("    … %d more", len(c.conflicted)-i)))
				break
			}
			lines = append(lines, "    "+st.text.Render(trunc(p)))
		}
		if len(c.merged) > 0 {
			lines = append(lines, "", st.add.Render(trunc("✓ Already merged: "+strings.Join(c.merged, ", "))))
		}
		if len(c.remaining) > 0 {
			lines = append(lines, st.muted.Render(trunc("Not merged yet: "+strings.Join(c.remaining, ", ")+". Review again once this is committed.")))
		}
		lines = append(lines, "", st.muted.Render(trunc("Resolve the files, then commit with : (git commit).")))
		if c.stage == stageAborting {
			lines = append(lines, "", spin+st.muted.Render("Aborting the merge…"))
		} else {
			keys = [][2]string{{"a", "ask the agent"}, {"e", "open in editor"}, {"x", "abort merge"}, {"esc", "leave it"}}
		}
	}
	body := st.bold.Render(title) + "\n\n" + strings.Join(lines, "\n")
	if len(keys) > 0 {
		body += "\n\n" + m.hints(keys)
	}
	return st.modal.Padding(1, 2).Width(width).Render(body)
}

// checkLine is one branch in the choose stage.
func (m *Model) checkLine(c mergeCheck, cursor bool) string {
	st := m.st
	mark := "  "
	if cursor {
		mark = st.accent.Render("› ")
	}
	box := st.faint.Render("[ ] ")
	if c.selected {
		box = st.accent.Bold(true).Render("[x] ")
	}
	name := st.bold.Render(c.name)
	var status string
	switch {
	case c.err != nil:
		box = "    "
		status = st.del.Render(" · couldn't check: " + c.err.Error())
	case c.commits == 0:
		box = "    "
		name = st.muted.Render(c.name)
		status = st.faint.Render(" · nothing new to merge")
	case len(c.paths) > 0:
		status = st.muted.Render(" · "+plural(c.commits, "commit")) + st.warn.Render(" · "+plural(len(c.paths), "conflict")+": "+strings.Join(c.paths, ", "))
	default:
		status = st.muted.Render(" · "+plural(c.commits, "commit")) + st.add.Render(" · clean")
	}
	if c.dirty && c.mergeable() {
		status += st.faint.Render(" · its uncommitted edits stay behind")
	}
	return mark + box + name + status
}
