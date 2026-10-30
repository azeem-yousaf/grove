package ui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
)

func plannerSays(block string) []agent.Event {
	return []agent.Event{{Kind: agent.EvText, Text: "Here's what I decided.\n\n```grove-orchestrate\n" + block + "\n```"}, {Kind: agent.EvDone}}
}

func TestPlannerDecidesRefusedCommands(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	// Both are the planner's to answer here.
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	fake.turns = [][]agent.Event{
		plannerSays(`{"approve": [{"branch": "feature/a", "command": "npm test"}],
			"deny": [{"branch": "feature/a", "command": "git push --force", "reason": "the user pushes"}]}`),
	}
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	s.running = true
	pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{
		{Tool: "Bash", Detail: "npm test"}, {Tool: "Bash", Detail: "git push --force"},
	}}))

	// The resumed agent then finishes, and the planner checks its work.
	if len(fake.reqs) != 3 || !strings.Contains(fake.reqs[2].Message, "feature/a (task \"Work on feature/a\") says it is done") {
		t.Fatalf("want a planner turn, the agent resumed and the planner checking it, got %d requests", len(fake.reqs))
	}
	ask := fake.reqs[0]
	if !ask.ReadOnly || !strings.Contains(ask.Message, ": `npm test`") || !strings.Contains(ask.Message, "- id "+s.answered[0].ID+": `npm test`") || !strings.Contains(ask.Message, "```grove-orchestrate") {
		t.Errorf("planner request:\n%s", ask.Message)
	}
	if last := m.ownerPlanner(s.path).entries[0]; last.role != roleNote || last.text != "Asked to decide: feature/a wants to run 2 commands" {
		t.Errorf("planner chat shows %+v", last)
	}
	if len(s.pending) != 0 || !slices.Contains(s.allow, agent.ClaudeRule("Bash", "npm test")) || slices.Contains(s.allow, agent.ClaudeRule("Bash", "git push --force")) {
		t.Errorf("pending %+v, allow %v", s.pending, s.allow)
	}
	resume := fake.reqs[1]
	if resume.Dir != s.path || !strings.Contains(resume.Message, "Approved: `npm test`") || !strings.Contains(resume.Message, "Denied: `git push --force` (the user pushes)") {
		t.Errorf("agent resumed with %q in %s", resume.Message, resume.Dir)
	}
}

func TestPlannerAnswersQuestionsAndIsAskedAgain(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	// It answers the question but says nothing about the command.
	fake.turns = [][]agent.Event{plannerSays(`{"reply": [{"branch": "feature/b", "message": "Use JSON."}]}`)}
	a, b := m.sessionFor(m.taskPath(p, "feature/a")), m.sessionFor(m.taskPath(p, "feature/b"))
	// Both arrive while the planner is busy, so one turn covers them.
	crew := m.plannerFor(p.ID, "g")
	crew.running = true
	b.lastReply.WriteString("Should the API return JSON or XML?")
	b.running = true
	m.onAgentDone(b, agent.Event{Kind: agent.EvDone})
	a.running = true
	m.onAgentDone(a, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: "make deploy"}}})
	crew.running = false
	pump(t, m, m.orchestrate())

	// The planner is asked again about the command; left twice, it is
	// denied and a goes on. b finishes, and the planner checks its work.
	if len(fake.reqs) < 3 || !strings.Contains(fake.reqs[0].Message, "JSON or XML?") || !strings.Contains(fake.reqs[0].Message, "make deploy") {
		t.Fatalf("one planner turn should cover both, got %d requests", len(fake.reqs))
	}
	has := func(dir, text string) bool {
		return slices.ContainsFunc(fake.reqs, func(r agent.Request) bool { return r.Dir == dir && strings.Contains(r.Message, text) })
	}
	if !has(b.path, "From the planner, who is watching this plan for the user: Use JSON.") {
		t.Error("the answer should go to b")
	}
	if !has(crew.path, "You didn't answer feature/a's `make deploy`") || !has(a.path, "Denied: `make deploy`") || !has(crew.path, "feature/b (task \"Work on feature/b\") says it is done") {
		t.Errorf("requests: %d", len(fake.reqs))
	}
	if len(a.pending) != 0 {
		t.Errorf("nothing should be left for the user: %+v", a.pending)
	}
}

func TestPlannerBatchesItemsThatArriveTogether(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.cfg.Planner.Settle = nil // the planner waits 5 s
	m.classify = func(*session, string, string) approvalPolicy { return approvalPolicy{} }
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	deny := func(branch, cmd string, d time.Duration) {
		m.now = t0.Add(d)
		s := m.sessionFor(m.taskPath(p, branch))
		s.running = true
		pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: cmd}}}))
	}
	deny("feature/a", "npm test", 0)
	deny("feature/b", "npm run lint", 2*time.Second)
	m.now = t0.Add(6 * time.Second)
	pump(t, m, m.orchestrate())
	if len(fake.reqs) != 0 {
		t.Fatalf("the planner should wait 5 s after the newest item: %d requests", len(fake.reqs))
	}
	fake.turns = [][]agent.Event{plannerSays(`{"approve": [{"branch": "feature/a", "command": "npm test"}, {"branch": "feature/b", "command": "npm run lint"}]}`)}
	m.now = t0.Add(7 * time.Second)
	pump(t, m, m.orchestrate())
	if len(fake.reqs) == 0 || !fake.reqs[0].ReadOnly || !strings.Contains(fake.reqs[0].Message, "`npm test`") || !strings.Contains(fake.reqs[0].Message, "`npm run lint`") {
		t.Fatalf("one planner turn should cover both: %+v", fake.reqs)
	}
	if n := len(slices.DeleteFunc(slices.Clone(fake.reqs), func(r agent.Request) bool { return !r.ReadOnly })); n != 1 {
		t.Errorf("%d planner turns, want 1", n)
	}
}

func TestUserPostReachesThePlannerAtOnce(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.cfg.Planner.Settle = nil
	m.now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	log, _ := m.groupLog(p, plan.ChatID)
	read := func() {
		m.pollingGroups = false
		_, cmd := m.Update(m.pollGroups()())
		pump(t, m, cmd)
	}
	read() // the first read is history
	fake.turns = [][]agent.Event{plannerSays(`{}`)}
	log.Post(group.Message{From: group.User, Text: "Please keep the old API too."})
	read()
	if len(fake.reqs) != 1 || !fake.reqs[0].ReadOnly || !strings.Contains(fake.reqs[0].Message, "Please keep the old API too.") {
		t.Fatalf("the user's post should reach the planner without waiting: %+v", fake.reqs)
	}
}

// A task whose turn ends without finishing or asking goes to the planner
// with its next turn, or on its own after idleWait.
func TestPlannerHearsAboutIdleTasks(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.now = time.Now()
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	// A suggested command only the user may run waits, so the task stays working.
	s.pending = []pendingAction{{kind: pendingCommand, text: "make deploy", policy: approvalPolicy{userOnly: true}}}
	s.lastReply.WriteString("Run make deploy for me.")
	s.running = true
	pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone}))
	if len(fake.reqs) != 0 || len(m.orch.queue) != 1 || m.orch.queue[0].kind != orchIdle || m.orch.queue[0].urgent {
		t.Fatalf("want one waiting idle item and no planner turn, got %d requests, queue %+v", len(fake.reqs), m.orch.queue)
	}
	// A second idle turn doesn't add another.
	s.running = true
	pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone}))
	if len(m.orch.queue) != 1 {
		t.Fatalf("queue %+v", m.orch.queue)
	}

	m.now = m.now.Add(idleWait - time.Second)
	pump(t, m, m.idleTick())
	if len(fake.reqs) != 0 {
		t.Fatal("it should wait idleWait")
	}
	m.now = m.now.Add(time.Second)
	pump(t, m, m.idleTick())
	if len(fake.reqs) != 1 || !strings.Contains(fake.reqs[0].Message, "feature/a (task \"Work on feature/a\") ended its turn without finishing") ||
		!strings.Contains(fake.reqs[0].Message, "> Run make deploy for me.") {
		t.Fatalf("planner requests: %+v", fake.reqs)
	}
	if len(m.orch.queue) != 0 {
		t.Errorf("queue left %+v", m.orch.queue)
	}
}

// A finished task that finishes again without a new commit isn't checked
// again; once its HEAD moves, it is.
func TestFinishedAgainNeedsANewCommit(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	finish := func() {
		t.Helper()
		s.running = true
		s.lastReply.Reset()
		s.lastReply.WriteString("Done.")
		pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone}))
	}
	checks := func() int {
		n := 0
		for _, r := range fake.reqs {
			if strings.Contains(r.Message, "feature/a (task \"Work on feature/a\") says it is done") {
				n++
			}
		}
		return n
	}
	finish()
	if _, task := m.taskFor(s.path); task.Status != plan.Finished || checks() != 1 || s.finishedHead == "" {
		t.Fatalf("status %v, checks %d, head %q", task.Status, checks(), s.finishedHead)
	}
	if r := restore(s.saved(), true, true); r.finishedHead != s.finishedHead {
		t.Errorf("restored head %q, want %q", r.finishedHead, s.finishedHead)
	}

	// Woken again (a group message), it says it's done with nothing new.
	finish()
	if checks() != 1 {
		t.Fatalf("checked %d times without a new commit", checks())
	}

	// A new commit is news.
	gitOut(t, s.path, "commit", "--allow-empty", "-qm", "more")
	finish()
	if checks() != 2 {
		t.Fatalf("checked %d times after a new commit", checks())
	}
}

func TestUnwatchedPlanWaitsForTheUser(t *testing.T) {
	m, p, fake := groupedPlan(t)
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	s.running = true
	pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: "npm test"}}}))
	if len(fake.reqs) != 0 || m.planner != nil || len(s.pending) != 1 {
		t.Errorf("requests %d, planner %v, pending %d", len(fake.reqs), m.planner != nil, len(s.pending))
	}
}

func TestPlannerPausesAtTheLimit(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	m.cfg.Planner.MaxDecisions = 1
	s := m.sessionFor(m.taskPath(p, "feature/a"))
	deny := func() {
		s.running = true
		pump(t, m, m.onAgentDone(s, agent.Event{Kind: agent.EvDone, Denied: []agent.Denial{{Tool: "Bash", Detail: "npm test"}}}))
	}
	deny()
	s.pending = nil
	deny()
	st := m.stateOf(m.ownerPlanner(s.path))
	if len(fake.reqs) != 1 || !st.paused {
		t.Fatalf("after the limit: %d planner turns, paused %v", len(fake.reqs), st.paused)
	}
	// Answering anything yourself lets it go on.
	m.chatPath, m.focus = s.path, focusChat
	press(m, "esc")
	if st.paused || st.turns != 0 {
		t.Error("the user's answer should reset the count")
	}
}

func TestPlannerFollowsGroupChat(t *testing.T) {
	m, p, fake := groupedPlan(t)
	p.Orchestrated = true
	log, _ := m.groupLog(p, "g")
	// Busy agents aren't woken for their messages, which would take the
	// planner's scripted reply.
	for _, b := range []string{"feature/a", "feature/b"} {
		m.sessionFor(m.taskPath(p, b)).running = true
	}
	read := func() {
		m.pollingGroups = false
		_, cmd := m.Update(m.pollGroups()())
		pump(t, m, cmd)
	}
	// The agents are woken for their messages too; only the planner's
	// requests matter here.
	planned := func() []agent.Request {
		var out []agent.Request
		for _, r := range fake.reqs {
			if r.ReadOnly {
				out = append(out, r)
			}
		}
		return out
	}
	log.Post(group.Message{From: "feature/a", To: "feature/b", Text: "old news"})
	read() // what was there before isn't passed on
	if len(planned()) != 0 {
		t.Fatal("history shouldn't wake the planner")
	}
	log.Post(group.Message{From: "feature/a", To: "feature/b", Text: "I renamed the type"})
	read()
	if len(planned()) != 0 || len(m.orch.queue) != 1 {
		t.Fatal("a message between two agents waits for the next planner turn")
	}
	fake.turns = [][]agent.Event{plannerSays(`{"post": [{"group": "g", "to": "feature/b", "message": "Use v2 of the API."}]}`)}
	log.Post(group.Message{From: "feature/b", Text: "Which API version do we target?"})
	read()
	if r := planned(); len(r) != 1 || !strings.Contains(r[0].Message, "I renamed the type") || !strings.Contains(r[0].Message, "Which API version") {
		t.Fatalf("planner requests: %+v", r)
	}
	msgs, _ := log.All()
	if last := msgs[len(msgs)-1]; last.From != group.PlannerOf("g") || last.To != "feature/b" || last.Text != "Use v2 of the API." {
		t.Errorf("last message = %+v", last)
	}
	if ctx := m.agentContext(m.sessionFor(m.taskPath(p, "feature/a"))); !strings.Contains(ctx, "Your group's planner is in charge of it") {
		t.Error("grouped agents should know the planner reads their chat")
	}
}

func TestAskWhoWatchesBeforePlanning(t *testing.T) {
	m := fakeModel(t, 140, 36)
	m.cfg.Planner.Watcher = ""
	var saved []string
	m.saveWatch = func(w string) error { saved = append(saved, w); return nil }
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	press(m, "P")
	if m.watchAsk == nil || m.focus == focusChat {
		t.Fatal("P should ask who watches before the planner opens")
	}
	if out := plain(m); !strings.Contains(out, "Who watches this plan's agents?") || !strings.Contains(out, "[ ] Remember my choice") {
		t.Errorf("question:\n%s", out)
	}
	press(m, "2")
	if m.watchAsk != nil || m.planWatch != config.WatchPlanner || m.focus != focusChat || len(saved) != 0 {
		t.Fatalf("after choosing: ask %v, watch %q, focus %v, saved %v", m.watchAsk, m.planWatch, m.focus, saved)
	}
	m.onPlannerReply(m.planner, "```grove-plan\n{\"title\": \"T\", \"tasks\": [{\"title\": \"A\", \"brief\": \"b\", \"branch\": \"feature/x\"}]}\n```")
	if d := m.draft(); d == nil || !d.Orchestrated || m.review == nil {
		t.Fatalf("the draft should be watched by the planner: %+v", d)
	}
	if out := plain(m); !strings.Contains(out, "Watched by the planner") {
		t.Errorf("review:\n%s", out)
	}
	press(m, "o")
	if m.review.p.Orchestrated || m.planWatch != config.WatchYou {
		t.Error("o should hand watching back to the user")
	}
	press(m, "esc", "esc")

	// Remembered, it isn't asked again.
	m.planWatch = ""
	press(m, "P")
	press(m, "space")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	pump(t, m, cmd)
	if len(saved) != 1 || saved[0] != config.WatchYou || m.cfg.Planner.Watcher != config.WatchYou {
		t.Errorf("saved %v, config %q", saved, m.cfg.Planner.Watcher)
	}
	m.planWatch = ""
	press(m, "esc", "P")
	if m.watchAsk != nil {
		t.Error("a remembered choice shouldn't be asked again")
	}
}

func TestStartedPlanAsksAgain(t *testing.T) {
	m := fakeModel(t, 140, 36)
	m.planWatch = config.WatchPlanner
	p := &plan.Plan{ID: "p", Title: "T", Status: plan.Draft, Orchestrated: true, Tasks: []plan.Task{{ID: "a", Branch: "feature/x"}}}
	m.plans = []*plan.Plan{p}
	m.startPlan(p, agent.Claude, m.planBranches)
	if m.planWatch != "" {
		t.Error("the next plan should be asked about again")
	}
}
