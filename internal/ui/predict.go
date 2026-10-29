package ui

import (
	"context"
	"slices"
	"sort"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/git"
)

// overlapView is what the o key narrows the list to.
type overlapView uint8

const (
	overlapsOff    overlapView = iota
	overlapsShown              // files changed in more than one worktree
	conflictsShown             // files predicted to conflict
)

// predictMsg is the result of predicting conflicts for one snapshot. Its
// indexes refer to snap, which is applied only while it is still m.snap.
type predictMsg struct {
	seq     int
	snap    *git.Snapshot
	preds   []git.Prediction
	checked bool // the git version was checked this time
	support bool // git can predict (merge-tree --write-tree)
}

// startPredict test-merges the worktrees that share files in the current
// snapshot, or queues another run if one is going.
func (m *Model) startPredict() tea.Cmd {
	if m.predictor == nil || m.snap == nil || m.removing != nil || m.predictOff ||
		m.repo.CommonDir == "" || !m.cfg.Conflicts.Enabled() {
		return nil
	}
	if m.predicting {
		m.repredict = true
		return nil
	}
	m.predicting = true
	m.predictSeq++
	seq, snap, check := m.predictSeq, m.snap, !m.predictChecked
	p, repo := m.predictor, m.repo
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 60*time.Second)
		defer cancel()
		msg := predictMsg{seq: seq, snap: snap, checked: check, support: true}
		if check {
			ok, err := git.MergeTreeSupported(ctx, repo.CommonDir)
			if msg.support = ok && err == nil; !msg.support {
				return msg
			}
		}
		msg.preds = p.Predict(ctx, repo, snap)
		return msg
	}
}

func (m *Model) onPredict(msg predictMsg) tea.Cmd {
	if msg.seq != m.predictSeq {
		return nil
	}
	m.predicting = false
	var cmds []tea.Cmd
	if msg.checked {
		m.predictChecked = true
		if !msg.support {
			m.predictOff, m.repredict = true, false
			m.predictions = nil
			return m.notify("Conflict prediction needs git 2.38 or later; ⚠ marks show overlaps only", toastInfo)
		}
	}
	// A newer scan replaced the snapshot; its own prediction is queued.
	if msg.snap == m.snap {
		prev := m.predictions
		m.predictions = msg.preds
		m.rebuildRows()
		cmds = append(cmds, m.onConflictsChanged(prev, m.predictions))
	}
	if m.repredict {
		m.repredict = false
		cmds = append(cmds, m.startPredict())
	}
	return tea.Batch(cmds...)
}

// remapPredictions moves predictions made for one snapshot onto the indexes
// of another, by worktree path, so they keep showing until the new
// snapshot's own predictions arrive. Worktrees that are gone drop out.
func remapPredictions(preds []git.Prediction, from, to *git.Snapshot) []git.Prediction {
	if from == nil || to == nil || from == to {
		return preds
	}
	var out []git.Prediction
	for _, p := range preds {
		if p.A >= len(from.States) || p.B >= len(from.States) {
			continue
		}
		a, b := to.IndexOf(from.States[p.A].Path), to.IndexOf(from.States[p.B].Path)
		if a < 0 || b < 0 {
			continue
		}
		if a > b {
			a, b = b, a
			files := make([]git.FileConflict, len(p.Files))
			for i, f := range p.Files {
				files[i] = git.FileConflict{Path: f.Path, A: f.B, B: f.A}
			}
			p.Files = files
		}
		p.A, p.B = a, b
		out = append(out, p)
	}
	return out
}

// conflictsFor lists the worktrees (indexes into m.snap.States) that path
// in worktree wt is predicted to conflict with.
func (m *Model) conflictsFor(wt int, path string) []int {
	var out []int
	for _, p := range m.predictions {
		if p.Err != nil || p.A != wt && p.B != wt {
			continue
		}
		if slices.ContainsFunc(p.Files, func(f git.FileConflict) bool { return f.Path == path }) {
			other := p.B
			if p.B == wt {
				other = p.A
			}
			out = append(out, other)
		}
	}
	sort.Ints(out)
	return out
}

// willConflict reports whether path in worktree wt is predicted to conflict.
func (m *Model) willConflict(wt int, path string) bool {
	return len(m.conflictsFor(wt, path)) > 0
}

// conflictLines are the lines of path, in worktree wt's copy, that clash
// with any other worktree.
func (m *Model) conflictLines(wt int, path string) []git.LineRange {
	var out []git.LineRange
	for _, p := range m.predictions {
		if p.Err != nil || p.A != wt && p.B != wt {
			continue
		}
		for _, f := range p.Files {
			if f.Path != path {
				continue
			}
			if p.A == wt {
				out = append(out, f.A...)
			} else {
				out = append(out, f.B...)
			}
		}
	}
	return out
}

// conflictPaths are the distinct files predicted to conflict anywhere.
func (m *Model) conflictPaths() map[string]bool {
	out := map[string]bool{}
	for _, p := range m.predictions {
		if p.Err != nil {
			continue
		}
		for _, f := range p.Files {
			out[f.Path] = true
		}
	}
	return out
}

// conflictCount is the number of distinct files predicted to conflict.
func (m *Model) conflictCount() int { return len(m.conflictPaths()) }

// cleanOverlapCount is the number of overlapping files not predicted to
// conflict: they merge cleanly, or nobody knows yet.
func (m *Model) cleanOverlapCount() int {
	if m.snap == nil {
		return 0
	}
	conf := m.conflictPaths()
	n := 0
	for path := range m.snap.Overlaps {
		if !conf[path] {
			n++
		}
	}
	return n
}

// conflictPairs are the pairs of worktrees predicted to conflict, with how
// many files each pair conflicts in.
func (m *Model) conflictPairs() []git.Prediction {
	var out []git.Prediction
	for _, p := range m.predictions {
		if p.Err == nil && len(p.Files) > 0 {
			out = append(out, p)
		}
	}
	return out
}
