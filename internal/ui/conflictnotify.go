package ui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/group"
)

// When conflict prediction finds that a plan task's worktree has started
// conflicting with another worktree, grove tells that task's agent: in its
// group chat, from group.Grove, when the task is grouped, otherwise as a
// note given with the agent's next turn. Each side of a pair is told once
// per set of files; m.conflictsTold remembers what, keyed by the two
// worktree paths, and forgets a pair once it stops conflicting, so a later
// conflict is told again.

// onConflictsChanged is called with the predictions before and after each
// prediction run, both indexing m.snap.States. What is new is worked out
// from m.conflictsTold rather than prev, so nothing is told twice even when
// a prediction run is repeated.
func (m *Model) onConflictsChanged(_, next []git.Prediction) tea.Cmd {
	if m.snap == nil {
		return nil
	}
	if m.conflictsTold == nil {
		m.conflictsTold = map[string][]string{}
	}
	states := m.snap.States
	seen := map[string]bool{}
	var cmds []tea.Cmd
	for _, pr := range next {
		if pr.A < 0 || pr.B < 0 || pr.A >= len(states) || pr.B >= len(states) {
			continue
		}
		a, b := states[pr.A], states[pr.B]
		for _, side := range []struct {
			self, other git.State
			isA         bool
		}{{a, b, true}, {b, a, false}} {
			key := side.self.Path + "\x00" + side.other.Path
			seen[key] = true
			if pr.Err != nil {
				continue // unknown this time: keep what was told
			}
			if len(pr.Files) == 0 {
				delete(m.conflictsTold, key)
				continue
			}
			files := make([]string, len(pr.Files))
			for i, f := range pr.Files {
				files[i] = f.Path
			}
			told := m.conflictsTold[key]
			m.conflictsTold[key] = files
			if !isSubset(files, told) {
				cmds = append(cmds, m.tellConflict(side.self, side.other, pr.Files, side.isA))
			}
		}
	}
	for key := range m.conflictsTold {
		if !seen[key] {
			delete(m.conflictsTold, key)
		}
	}
	return tea.Batch(cmds...)
}

func isSubset(sub, of []string) bool {
	for _, s := range sub {
		if !slices.Contains(of, s) {
			return false
		}
	}
	return true
}

// tellConflict tells the agent of the task in self, if any, that it now
// conflicts with other. isA says which side of files' ranges is self's.
func (m *Model) tellConflict(self, other git.State, files []git.FileConflict, isA bool) tea.Cmd {
	p, t := m.taskFor(self.Path)
	if t == nil {
		return nil
	}
	op, ot := m.taskFor(other.Path)
	groupmate := t.Group != "" && ot != nil && op == p && ot.Group == t.Group
	text := conflictText(other, files, isA, t.Group != "", groupmate)
	if t.Group != "" {
		if log, ok := m.groupLog(p, t.Group); ok {
			msg := group.Message{From: group.Grove, To: t.Branch, Text: text}
			return func() tea.Msg {
				if err := log.Post(msg); err != nil {
					return actionMsg{err: fmt.Errorf("couldn't tell %s about a conflict: %w", msg.To, err)}
				}
				return groupPostedMsg{} // reads the group, which wakes the agent
			}
		}
	}
	s := m.sessionFor(self.Path)
	if s == nil {
		s = m.newSession(t.Path, t.Branch, agent.Kind(p.Agent))
	}
	s.notes = append(s.notes, text)
	s.entries = append(s.entries, chatEntry{roleNote, fmt.Sprintf("⚠ This worktree now conflicts with %s in %s. The agent will be told with its next message.",
		other.Name(), plural(len(files), "file"))})
	return nil
}

// conflictText is what an agent is told: what it conflicts with, the files,
// the lines in its own version, and what to do about it.
func conflictText(other git.State, files []git.FileConflict, isA, grouped, groupmate bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your worktree now conflicts with %s (worktree %s): merging the two would conflict in these files. Line numbers are in your version.\n", other.Name(), other.Path)
	for i, f := range files {
		if i == 20 {
			fmt.Fprintf(&b, "- and %d more\n", len(files)-20)
			break
		}
		mine := f.B
		if isA {
			mine = f.A
		}
		fmt.Fprintf(&b, "- %s: %s\n", f.Path, lineRanges(mine))
	}
	switch {
	case groupmate:
		fmt.Fprintf(&b, "Agree with %s here who changes these lines, or avoid changing them if your task allows.", other.Name())
	case grouped:
		fmt.Fprintf(&b, "%s isn't in your group: avoid changing these lines if your task allows, or tell the user about the conflict.", other.Name())
	default:
		b.WriteString("That worktree is used for other work: avoid changing these lines if your task allows, or tell the user about the conflict.")
	}
	return b.String()
}

func lineRanges(rs []git.LineRange) string {
	if rs == nil {
		return "you deleted it"
	}
	if len(rs) == 0 {
		return "the whole file"
	}
	parts := make([]string, len(rs))
	for i, r := range rs {
		if r.End <= r.Start {
			parts[i] = fmt.Sprint(r.Start)
		} else {
			parts[i] = fmt.Sprintf("%d-%d", r.Start, r.End)
		}
	}
	if len(rs) == 1 && rs[0].End <= rs[0].Start {
		return "line " + parts[0]
	}
	return "lines " + strings.Join(parts, ", ")
}

// takeNotes gives the agent grove's pending notes with this turn: in the
// briefing on the first turn, otherwise ahead of the message, since the
// briefing isn't sent again once the conversation has started.
func (s *session) takeNotes(context, message string) (string, string) {
	if len(s.notes) == 0 {
		return context, message
	}
	news := strings.Join(s.notes, "\n\n")
	s.notes = nil
	if context != "" && s.id == "" && len(s.history) == 0 {
		return context + "\n## News from grove\n\n" + news + "\n", message
	}
	return context, "Note from grove (not the user):\n\n" + news + "\n\n---\n\n" + message
}
