package ui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/group"
)

// stateIndex is the m.snap.States index of branch.
func stateIndex(t *testing.T, m *Model, branch string) int {
	t.Helper()
	for i, st := range m.snap.States {
		if st.Branch == branch {
			return i
		}
	}
	t.Fatalf("no worktree on %s", branch)
	return -1
}

func conflict(a, b int, paths ...string) git.Prediction {
	p := git.Prediction{A: a, B: b}
	for _, path := range paths {
		p.Files = append(p.Files, git.FileConflict{Path: path, A: []git.LineRange{{Start: 2, End: 3}}, B: []git.LineRange{{Start: 5, End: 5}}})
	}
	return p
}

// groveMessages are the messages grove posted in the group.
func groveMessages(t *testing.T, log group.Log) []group.Message {
	t.Helper()
	all, err := log.All()
	if err != nil {
		t.Fatal(err)
	}
	var out []group.Message
	for _, msg := range all {
		if msg.From == group.Grove {
			out = append(out, msg)
		}
	}
	return out
}

func TestGroupedTasksHearOfNewConflictsOnce(t *testing.T) {
	m, p, fake := groupedPlan(t)
	log, _ := m.groupLog(p, "g")
	a, b := stateIndex(t, m, "feature/a"), stateIndex(t, m, "feature/b")
	next := []git.Prediction{conflict(a, b, "shared.txt")}

	// Count wake-ups from 1, to see grove's posts don't reset them.
	m.groups[log.Dir] = &groupState{wakeups: 1}
	var pollAgain tea.Cmd
	for _, msg := range runCmds(m.onConflictsChanged(nil, next)) {
		if _, ok := msg.(groupPostedMsg); ok {
			if _, c := m.Update(msg); c != nil {
				pollAgain = c
			}
		}
	}
	msgs := groveMessages(t, log)
	if len(msgs) != 2 {
		t.Fatalf("grove posted %d messages, want one to each side: %+v", len(msgs), msgs)
	}
	for _, msg := range msgs {
		switch msg.To {
		case "feature/a":
			if !strings.Contains(msg.Text, "conflicts with feature/b") || !strings.Contains(msg.Text, "- shared.txt: lines 2-3") || !strings.Contains(msg.Text, "Agree with feature/b") {
				t.Errorf("a was told:\n%s", msg.Text)
			}
		case "feature/b":
			if !strings.Contains(msg.Text, "conflicts with feature/a") || !strings.Contains(msg.Text, "- shared.txt: line 5") {
				t.Errorf("b was told:\n%s", msg.Text)
			}
		default:
			t.Errorf("message to %q", msg.To)
		}
		if msg.For(group.Grove) || !msg.For(msg.To) {
			t.Errorf("%+v should be for its member only", msg)
		}
	}

	// Posting reads the group again, which wakes both agents, as a post
	// would, counting wake-ups rather than resetting them.
	if pollAgain == nil {
		t.Fatal("posting should read the group again")
	}
	_, wake := m.Update(pollAgain())
	pump(t, m, wake)
	g := m.groups[log.Dir]
	if len(fake.reqs) != 2 || g.wakeups != 3 {
		t.Errorf("woken %d agents, wake-ups %d; want both woken and the count at 3", len(fake.reqs), g.wakeups)
	}

	// The same conflict again is not news.
	if cmd := m.onConflictsChanged(next, next); cmd != nil {
		runCmds(cmd)
	}
	if n := len(groveMessages(t, log)); n != 2 {
		t.Errorf("a repeat posted again: %d messages", n)
	}

	// The group pane shows grove's messages under its own name.
	open := m.openGroupChat(m.taskPath(p, "feature/a"))
	drive(t, m).until(open, func() bool { return !m.pollingGroups })
	if out := plain(m); !strings.Contains(out, "grove → feature/a") {
		t.Errorf("group chat:\n%s", out)
	}
}

func TestUngroupedTaskGetsConflictNoteWithNextTurn(t *testing.T) {
	m, p, fake := groupedPlan(t)
	c, mainIdx := stateIndex(t, m, "feature/c"), stateIndex(t, m, "main")
	s := m.sessionFor(m.taskPath(p, "feature/c"))
	turn := func(text string) string {
		t.Helper()
		pump(t, m, m.send(s, text))
		r := fake.reqs[len(fake.reqs)-1]
		return r.Context + "\n" + r.Message
	}

	// c conflicts with main, which runs no task (and is where the planner
	// works): only c is told.
	m.onConflictsChanged(nil, []git.Prediction{conflict(mainIdx, c, "c.txt")})
	if len(s.notes) != 1 || !strings.Contains(s.notes[0], "conflicts with main") || !strings.Contains(s.notes[0], "- c.txt: line 5") {
		t.Fatalf("c's notes = %q", s.notes)
	}
	if ms := m.sessionFor(m.snap.States[mainIdx].Path); ms != nil && len(ms.notes) > 0 {
		t.Error("main runs no task and must not be told")
	}
	if m.planner != nil && len(m.planner.notes) > 0 {
		t.Error("the planner must not be told")
	}
	if fake.reqs != nil {
		t.Fatal("an isolated agent is told with its next turn, not woken")
	}
	// Its first turn: in the briefing.
	got := turn("hello")
	if !strings.Contains(got, "## News from grove") || !strings.Contains(got, "conflicts with main") {
		t.Errorf("first turn:\n%s", got)
	}
	if got := turn("again"); strings.Contains(got, "conflicts with") {
		t.Errorf("the note was given twice:\n%s", got)
	}

	// The same conflict is not news; more files are.
	m.onConflictsChanged(nil, []git.Prediction{conflict(mainIdx, c, "c.txt")})
	if len(s.notes) != 0 {
		t.Fatalf("a repeat made notes: %q", s.notes)
	}
	m.onConflictsChanged(nil, []git.Prediction{conflict(mainIdx, c, "c.txt", "d.txt")})
	if len(s.notes) != 1 || !strings.Contains(s.notes[0], "- d.txt") {
		t.Fatalf("a growing file set should be told: %q", s.notes)
	}
	// Later turns get it ahead of the message, since the briefing isn't sent.
	s.id = "sess-1"
	got = turn("carry on")
	if !strings.Contains(got, "Note from grove") || !strings.Contains(got, "- d.txt") || !strings.HasSuffix(got, "carry on") {
		t.Errorf("later turn:\n%s", got)
	}

	// Resolved, then conflicting again: told again.
	m.onConflictsChanged(nil, nil)
	if len(m.conflictsTold) != 0 {
		t.Errorf("resolved pairs should be forgotten: %v", m.conflictsTold)
	}
	m.onConflictsChanged(nil, []git.Prediction{conflict(mainIdx, c, "c.txt")})
	if len(s.notes) != 1 {
		t.Fatalf("a new conflict after resolving should be told: %q", s.notes)
	}
	// A run that failed for the pair keeps what was told.
	m.onConflictsChanged(nil, []git.Prediction{{A: mainIdx, B: c, Err: errors.New("merge-tree failed")}})
	m.onConflictsChanged(nil, []git.Prediction{conflict(mainIdx, c, "c.txt")})
	if len(s.notes) != 1 {
		t.Errorf("an unknown result shouldn't make the conflict news again: %q", s.notes)
	}

	// Notes not yet given survive a restart.
	if r := restore(s.saved(), true, false); len(r.notes) != 1 || r.notes[0] != s.notes[0] {
		t.Errorf("restored notes = %q", r.notes)
	}
}
