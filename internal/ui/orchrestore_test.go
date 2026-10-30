package ui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/usage"
)

// restart opens m's repository again with a fake agent, loading the saved
// plans and sessions in the order given, and pumps what that starts.
func restart(t *testing.T, m *Model, plansFirst bool) (*Model, *fakeAgent) {
	t.Helper()
	m.persistNow()
	repo := *m.repo
	m2 := New(context.Background(), Options{Repo: &repo, Config: config.Default(), Version: "test"})
	noAgents(m2)
	fake := &fakeAgent{}
	m2.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	m2.runAgent = fake.run
	m2.groveBin = m.groveBin
	m2.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	m2.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m2.Update(baseMsg{base: "main"})
	m2.Update(scanMsg{seq: m2.scanSeq, snap: m.snap})
	load := []func() tea.Msg{m2.loadPlans(), m2.loadSessions()}
	if !plansFirst {
		load[0], load[1] = load[1], load[0]
	}
	var cmds []tea.Cmd
	for _, l := range load {
		_, cmd := m2.Update(l())
		cmds = append(cmds, cmd)
	}
	pump(t, m2, tea.Batch(cmds...))
	return m2, fake
}

func askedAll(fake *fakeAgent) string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var b strings.Builder
	for _, r := range fake.reqs {
		if r.ReadOnly {
			b.WriteString(r.Message + "\n")
		}
	}
	return b.String()
}

// What waited for the planners when grove closed reaches them after it
// opens again, whichever of plans and sessions loads first.
func TestPlannerQueueSurvivesRestart(t *testing.T) {
	for _, plansFirst := range []bool{true, false} {
		m, p, _ := groupedPlan(t)
		p.Orchestrated = true
		m.plansLoaded = true // as if read at start, so the read positions are saved
		m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
		a := m.sessionFor(m.taskPath(p, "feature/a"))
		a.pending = []pendingAction{m.newApproval(a, "Bash", "make deploy")}
		c := m.sessionFor(m.taskPath(p, "feature/c"))
		c.entries = []chatEntry{{roleUser, "Start"}, {roleAgent, "I have two options. Please confirm which format to use: JSON or XML."}}
		_, tb := m.taskFor(m.taskPath(p, "feature/b"))
		tb.Status = plan.Finished
		// The chats are read once (their history), then the user posts
		// while grove is closed.
		m.pollingGroups = false
		m.Update(m.pollGroups()())
		if len(m.orch.told) == 0 {
			t.Fatal("the chats' history should count as told")
		}
		m.persistNow()
		log, _ := m.groupLog(p, plan.ChatID)
		log.Post(group.Message{From: group.User, Text: "Keep the old API."})

		m2, fake := restart(t, m, plansFirst)
		m2.pollingGroups = false
		_, cmd := m2.Update(m2.pollGroups()())
		pump(t, m2, cmd)
		asked := askedAll(fake)
		for _, want := range []string{"`make deploy`", "Please confirm which format", `feature/b (task "Work on feature/b") says it is done`, "Keep the old API."} {
			if !strings.Contains(asked, want) {
				t.Errorf("plans first %v: the planners weren't asked about %q:\n%s", plansFirst, want, asked)
			}
		}
	}
}

// A plan whose tasks all finished before grove closed is merged after it
// opens; work the planner already checked isn't checked again.
func TestSettledPlanIsMergedAfterRestart(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	for i := range p.Tasks {
		task := &p.Tasks[i]
		task.Status = plan.Finished
		m.sessionFor(task.Path).finishedHead = git.RefHash(context.Background(), task.Path, "HEAD")
	}
	_, fake := restart(t, m, true)
	asked := askedAll(fake)
	if !strings.Contains(asked, "Every task of this plan is finished or failed") {
		t.Errorf("the lead should be asked to merge:\n%s", asked)
	}
	if strings.Contains(asked, "says it is done") {
		t.Errorf("checked work shouldn't be checked again:\n%s", asked)
	}
}

// News saved nowhere else (how a merge went, review comments, a check a
// planner asked for) reaches the planner after a restart, whether it was
// still queued or in the planner's turn when grove closed.
func TestMergeNewsSurvivesRestart(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	m.plansLoaded = true // as if read at start, so orchFile is written
	m.queueMerge(p, "MERGE-NEWS: the plan merged cleanly.")
	m.orch.queue = append(m.orch.queue, orchItem{kind: orchReview, plan: p.ID, lines: []string{"REVIEW-NEWS: rename the flag"}, urgent: true})
	m.orch.sent = []orchItem{{kind: orchCheck, plan: p.ID, branch: "feature/a", lines: []string{"CHECK-NEWS: it passed"}, urgent: true}}

	_, fake := restart(t, m, false)
	asked := askedAll(fake)
	for _, want := range []string{"MERGE-NEWS", "REVIEW-NEWS", "CHECK-NEWS"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the planner wasn't told %s after the restart:\n%s", want, asked)
		}
	}
}

// Usage a removed agent hadn't written to the ledger survives grove
// closing: kept in usageLeftFile, and for an agent whose folder went while
// grove was closed, taken from its session file before that is deleted.
func TestRemovedAgentsUsageSurvivesRestart(t *testing.T) {
	m, _, _ := groupedPlan(t)
	m.sessionsLoaded = true // as if read at start, so usageLeftFile is written
	m.usageLeft = append(m.usageLeft, &session{uid: "removed", unrecorded: []usage.Turn{
		{ID: "removed/1", Agent: "removed", Tokens: usage.Tokens{Input: 5}, Status: usage.Exact}}})
	gone := m.newSession(filepath.Join(t.TempDir(), "gone"), "feature/gone", agent.Claude)
	gone.unrecorded = []usage.Turn{{ID: gone.uid + "/1", Agent: gone.uid, Tokens: usage.Tokens{Input: 7}, Status: usage.Exact}}
	m.sessions[gone.path] = gone
	m.persistNow()
	delete(m.sessions, gone.path) // its folder never existed, as if removed while grove was closed

	m2, _ := restart(t, m, true)
	writeUsage(t, m2)
	if sum := m2.usage.Agent("removed"); sum.Turns != 1 || sum.Tokens.Total() != 5 {
		t.Errorf("removed agent's usage = %+v", sum)
	}
	if sum := m2.usage.Agent(gone.uid); sum.Turns != 1 || sum.Tokens.Total() != 7 {
		t.Errorf("vanished agent's usage = %+v", sum)
	}
	if len(m2.usageLeft) != 0 {
		t.Errorf("usage left after it was written: %v", m2.usageLeft)
	}
	m2.persistNow()
	var left []savedLeft
	if err := m2.store.Load(usageLeftFile, &left); err != nil || len(left) != 0 {
		t.Errorf("%s = %+v, %v; want it empty", usageLeftFile, left, err)
	}
}
