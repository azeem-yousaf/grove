package ui

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/charmbracelet/x/ansi"
)

// removal is a worktree the user asked to remove, from the confirmation
// dialog through to git finishing.
type removal struct {
	st           git.State
	dirty        int  // files with uncommitted or untracked changes
	canBranch    bool // the branch may be offered for deletion
	deleteBranch bool
	force        bool
	nudged       bool // enter was pressed before ticking a required box
	queued       bool // waiting for a running scan to finish first
}

type removeMsg struct {
	r   *removal
	res git.RemoveResult
	err error
}

// needsForce reports whether git will refuse without --force.
func (r *removal) needsForce() bool {
	return !r.st.Prunable && (r.dirty > 0 || r.st.Truncated || r.st.Locked)
}

// askRemove opens the confirmation dialog for the selected worktree.
func (m *Model) askRemove() tea.Cmd {
	row, ok := m.selected()
	if !ok {
		return nil
	}
	if m.removing != nil {
		return m.notify("Still removing "+m.removing.st.Name(), toastInfo)
	}
	st := m.snap.States[row.wt]
	if st.Main || st.Bare {
		return m.notify("The main worktree holds the repository itself, so it can't be removed", toastInfo)
	}
	r := &removal{st: st, canBranch: st.Branch != "" && st.Branch != m.base && !st.Detached}
	for _, f := range st.Files {
		if f.Uncommitted {
			r.dirty++
		}
	}
	m.confirm = r
	return nil
}

func (m *Model) onConfirmKey(msg tea.KeyPressMsg) tea.Cmd {
	r := m.confirm
	switch msg.String() {
	case "esc", "q", "n":
		m.confirm = nil
	case "ctrl+c":
		return m.quitNow()
	case "b":
		if r.canBranch {
			r.deleteBranch = !r.deleteBranch
		}
	case "f":
		if r.needsForce() {
			r.force = !r.force
			r.nudged = false
		}
	case "enter", "y":
		if r.needsForce() && !r.force {
			r.nudged = true
			return nil
		}
		m.confirm = nil
		m.removing = r
		if m.scanning {
			// A scan running git inside the folder would keep files open.
			r.queued = true
			return nil
		}
		return m.runRemoval()
	}
	return nil
}

func (m *Model) runRemoval() tea.Cmd {
	r := m.removing
	r.queued = false
	repo := m.repo
	opt := git.RemoveOptions{Force: r.force, DeleteBranch: r.deleteBranch}
	// The worktree's agent works in the folder, which Windows won't delete
	// while a process is using it.
	dir := r.st.Path
	if s := m.sessionFor(dir); s != nil {
		dir = s.path // how the agent knows the folder
		if s.running && s.cancel != nil {
			s.cancel()
		}
	}
	stop, check := m.stopAgent, m.stopCheck(r.st.Path)
	run := func() tea.Msg {
		stop(dir)
		check()
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Minute)
		defer cancel()
		res, err := git.RemoveWorktree(ctx, repo, r.st.Worktree, opt)
		return removeMsg{r, res, err}
	}
	return tea.Batch(run, m.startAnimating())
}

func (m *Model) onRemoved(msg removeMsg) tea.Cmd {
	m.removing = nil
	name := msg.r.st.Name()
	if msg.err != nil {
		return tea.Batch(m.notify(fmt.Sprintf("Couldn't remove %s: %v", name, msg.err), toastErr), m.startScan())
	}
	if m.repo.Here != "" && git.SamePath(m.repo.Here, msg.r.st.Path) {
		// The worktree grove started in is gone; keep going from the repo.
		m.repo.Here = ""
		m.hereIdx = -1
	}
	delete(m.collapsed, msg.r.st.Path)
	forget := m.forgetSession(msg.r.st.Path)

	var note toast
	switch {
	case msg.res.BranchErr != nil:
		note = toast{fmt.Sprintf("Removed %s, but couldn't delete its branch: %v", name, msg.res.BranchErr), toastErr, 0}
	case msg.res.FolderLeft:
		note = toast{fmt.Sprintf("Removed %s; its empty folder is still open in another program, delete it later", name), toastErr, 0}
	case msg.r.deleteBranch:
		note = toast{fmt.Sprintf("Removed %s and deleted its branch", name), toastOK, 0}
	case msg.r.st.Branch != "":
		note = toast{fmt.Sprintf("Removed %s · branch kept", name), toastOK, 0}
	default:
		note = toast{"Removed " + name, toastOK, 0}
	}
	return tea.Batch(m.notify(note.text, note.kind), m.startScan(), forget)
}

// removeModal is the confirmation dialog.
func (m *Model) removeModal() string {
	st := m.st
	r := m.confirm
	w := min(66, m.width-10)
	para := func(s lipgloss.Style, text string) string { return s.Width(w).Render(text) }
	box := func(on bool) string {
		if on {
			return st.del.Bold(true).Render("[x]")
		}
		return st.faint.Render("[ ]")
	}

	name := r.st.Name()
	parts := []string{
		st.bold.Foreground(st.p.del).Render("Remove worktree"),
		"",
		st.bold.Foreground(st.p.accent).Render(name),
		st.faint.Render(shortPath(r.st.Path, w)),
		"",
	}

	switch {
	case r.st.Prunable:
		parts = append(parts, para(st.muted, "Its folder is already gone. This clears git's record of it."))
	case r.deleteBranch:
		parts = append(parts, para(st.muted, "The folder and the branch "+r.st.Branch+" will be deleted."))
	case r.st.Branch != "":
		parts = append(parts, para(st.muted, "The folder will be deleted. The branch "+r.st.Branch+" and its commits are kept, so you can check it out again later."))
	default:
		parts = append(parts, para(st.muted, "The folder will be deleted."))
	}

	var warns []string
	if r.dirty > 0 || r.st.Truncated {
		n := plural(r.dirty, "file")
		if r.st.Truncated {
			n = "Many files"
		}
		warns = append(warns, st.warn.Render("● ")+st.text.Render(n+" with uncommitted changes"))
	}
	if r.st.Locked {
		reason := ""
		if r.st.LockReason != "" {
			reason = ": " + r.st.LockReason
		}
		warns = append(warns, st.warn.Render("⚿ ")+st.text.Render("Locked"+reason))
	}
	if m.repo.Here != "" && git.SamePath(m.repo.Here, r.st.Path) && runtime.GOOS == "windows" {
		warns = append(warns, para(st.faint, "◆ grove was started in this folder. If a terminal is still in it, Windows keeps the empty folder until you leave."))
	}
	if len(warns) > 0 {
		parts = append(parts, "")
		parts = append(parts, warns...)
	}

	var opts []string
	if r.needsForce() {
		label := "Discard uncommitted changes"
		switch {
		case r.st.Locked && r.dirty == 0 && !r.st.Truncated:
			label = "Override the lock"
		case r.st.Locked:
			label = "Discard changes and override the lock"
		}
		line := st.key.Render("f") + "  " + box(r.force) + " " + st.text.Render(label)
		if r.nudged {
			line += st.del.Render("  ← needed to remove")
		}
		opts = append(opts, line)
	}
	if r.canBranch {
		line := st.key.Render("b") + "  " + box(r.deleteBranch) + " " + st.text.Render("Also delete branch "+r.st.Branch)
		switch {
		case r.st.Base != "" && r.st.Ahead > 0:
			line += st.del.Render(fmt.Sprintf("  %s not in %s", plural(r.st.Ahead, "commit"), r.st.Base))
		case r.st.Base != "":
			line += st.faint.Render("  nothing unmerged")
		}
		opts = append(opts, line)
	}
	if len(opts) > 0 {
		parts = append(parts, "")
		parts = append(parts, opts...)
	}

	action := "remove"
	if r.force {
		action = "remove and discard"
	}
	parts = append(parts, "", m.hints([][2]string{{"enter", action}, {"esc", "cancel"}}))

	// Keep every line inside the box, and drop the breathing room on short
	// terminals so the options and the hint stay visible.
	var lines []string
	for _, l := range strings.Split(strings.Join(parts, "\n"), "\n") {
		lines = append(lines, ansi.Truncate(l, w, "…"))
	}
	padY := 1
	if len(lines)+4 > m.height-2 {
		padY = 0
		kept := lines[:0]
		for _, l := range lines {
			if strings.TrimSpace(ansi.Strip(l)) != "" {
				kept = append(kept, l)
			}
		}
		lines = kept
	}
	return st.modal.BorderForeground(st.p.del).Padding(padY, 3).Render(strings.Join(lines, "\n"))
}
