package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/group"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// scripted answers each turn with what reply returns for it, after doing
// whatever reply does to the worktree.
type scripted struct {
	mu    sync.Mutex
	reqs  []agent.Request
	reply func(req agent.Request) string
}

func (f *scripted) run(_ context.Context, req agent.Request, emit func(agent.Event)) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if text := f.reply(req); text != "" {
		emit(agent.Event{Kind: agent.EvText, Text: text})
	}
	emit(agent.Event{Kind: agent.EvDone})
}

func (f *scripted) requests() []agent.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Request(nil), f.reqs...)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPlannerMergesThePlanAndWaitsForFeedback(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated, p.Base = true, "main"
	for i := range p.Tasks {
		p.Tasks[i].Status = plan.Finished
	}
	bPath, cPath := m.taskPath(p, "feature/b"), m.taskPath(p, "feature/c")
	// c leaves its work uncommitted, which grove commits before merging.
	if err := os.WriteFile(filepath.Join(cPath, "c2.txt"), []byte("more c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &scripted{reply: func(req agent.Request) string {
		switch {
		case req.ReadOnly && strings.Contains(req.Message, "is finished or failed"):
			return "```grove-orchestrate\n{\"merge\": [{\"plan\": \"p1\"}]}\n```"
		case req.ReadOnly && strings.Contains(req.Message, "wrong colour"):
			return "```grove-orchestrate\n{\"reply\": [{\"branch\": \"feature/c\", \"message\": \"Make it blue.\"}]}\n```"
		case req.ReadOnly && strings.Contains(req.Message, "Merged 3 tasks") && strings.Contains(req.Message, "Merged now: feature/c"):
			return "```grove-orchestrate\n{\"close\": [{\"plan\": \"p1\", \"note\": \"the user is happy\"}]}\n```"
		case req.ReadOnly && strings.Contains(req.Message, "Merged 3 tasks"):
			return "```grove-orchestrate\n{\"post\": [{\"plan\": \"p1\", \"group\": \"plan\", \"message\": \"All merged; try checkout.\"}]}\n```"
		case req.Dir == bPath && strings.Contains(req.Message, "conflicts in: shared.txt"):
			os.WriteFile(filepath.Join(bPath, "shared.txt"), []byte("one\nTWO from a, two from b\nthree\n"), 0o644)
			return "Done: kept both changes."
		case req.Dir == cPath && strings.Contains(req.Message, "Make it blue"):
			os.WriteFile(filepath.Join(cPath, "c.txt"), []byte("blue\n"), 0o644)
			return "Done: it is blue."
		}
		return ""
	}}
	m.runAgent = f.run
	planLog, _ := m.groupLog(p, plan.ChatID)
	chat := func() []string {
		msgs, _ := planLog.All()
		var out []string
		for _, msg := range msgs {
			out = append(out, msg.From+": "+msg.Text)
		}
		return out
	}

	// Once every task is settled the plan stays running, and the planner is
	// asked to merge it; b conflicts with a, so b's agent resolves that and
	// grove merges again.
	m.finishIfSettled(p)
	if p.Status != plan.Running {
		t.Fatalf("a watched plan waits for the planner to merge it: %s", p.Status)
	}
	settle(t, m, m.orchestrate(), func() bool {
		return p.Status == plan.Review && len(chat()) >= 3 && !m.planner.running
	})
	mg := p.Merge
	if mg == nil || mg.Branch != "plan/shop" || mg.Fixing != "" || !strings.HasSuffix(mg.Path, "shop-plan-shop") {
		t.Fatalf("merge = %+v", mg)
	}
	if got := gitOut(t, mg.Path, "show", "HEAD:shared.txt"); got != "one\nTWO from a, two from b\nthree" {
		t.Errorf("merged shared.txt = %q", got)
	}
	for _, f := range []string{"c.txt", "c2.txt"} {
		if _, err := os.Stat(filepath.Join(mg.Path, f)); err != nil {
			t.Errorf("%s should be merged: %v", f, err)
		}
	}
	if st := gitOut(t, cPath, "status", "--porcelain"); st != "" {
		t.Errorf("c's work should be committed: %q", st)
	}
	got := strings.Join(chat(), "\n")
	for _, want := range []string{"grove: feature/b conflicts with the work merged so far after merging feature/a, in shared.txt", "grove: Merged 3 tasks into plan/shop", "planner: All merged; try checkout."} {
		if !strings.Contains(got, want) {
			t.Errorf("plan chat is missing %q:\n%s", want, got)
		}
	}
	if b, _ := p.Task("feature/b"); b.Status != plan.Finished {
		t.Errorf("b = %+v", b)
	}
	if !m.agentRequest(m.planner).Commands {
		t.Error("the planner may still run commands while the plan is in review")
	}
	if p := m.mergedInto(mg.Path); p == nil || p.ID != "p1" {
		t.Error("the plan's worktree should lead back to the plan")
	}
	if items := m.attentionItems(); !slices.ContainsFunc(items, func(a attention) bool {
		return strings.Contains(a.text, "Shop is merged into plan/shop and waits for your feedback") && a.path == mg.Path
	}) {
		t.Errorf("the overview should point to the merged plan: %+v", items)
	}

	// The user's feedback in the plan chat reopens c through the planner;
	// once it is finished the planner merges again and closes the plan.
	m.orch.seen[planLog.Dir] = len(chat())
	if err := planLog.Post(group.Message{From: group.User, Text: "The button is the wrong colour."}); err != nil {
		t.Fatal(err)
	}
	m.pollingGroups = false
	settle(t, m, m.pollGroups(), func() bool {
		c := chat()
		return p.Status == plan.Done && !m.planner.running && strings.Contains(c[len(c)-1], "is closed")
	})
	if got := gitOut(t, mg.Path, "show", "HEAD:c.txt"); got != "blue" {
		t.Errorf("merged c.txt = %q", got)
	}
	if last := chat()[len(chat())-1]; !strings.Contains(last, "Shop is closed: the user is happy.") || !strings.Contains(last, "plan/shop") {
		t.Errorf("last message = %q", last)
	}
	if m.agentRequest(m.planner).Commands {
		t.Error("a closed plan gives the planner nothing more")
	}
}

func TestMergeWaitsForEveryTask(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated = true
	f := &scripted{reply: func(agent.Request) string { return "" }}
	m.runAgent = f.run
	settle(t, m, m.mergePlan(p), func() bool { return m.planner != nil && !m.planner.running && len(f.requests()) > 0 })
	if p.Merge != nil || p.Status != plan.Running {
		t.Fatalf("nothing should be merged while tasks work: %+v", p)
	}
	if ask := f.requests()[0].Message; !strings.Contains(ask, "grove couldn't merge the plan: some tasks are still working") {
		t.Errorf("planner request:\n%s", ask)
	}
}

func TestFailingTestsGoBackToTheAgents(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated, p.Base = true, "main"
	m.cfg.Planner.Test = "grep -q blue c.txt"
	if runtime.GOOS == "windows" {
		m.cfg.Planner.Test = "findstr blue c.txt"
	}
	// Only c merges; a and b failed.
	for i := range p.Tasks {
		p.Tasks[i].Status = plan.Failed
		if p.Tasks[i].Branch == "feature/c" {
			p.Tasks[i].Status = plan.Finished
		}
	}
	cPath := m.taskPath(p, "feature/c")
	f := &scripted{reply: func(req agent.Request) string {
		switch {
		case req.ReadOnly && strings.Contains(req.Message, "is finished or failed"):
			return "```grove-orchestrate\n{\"merge\": [{\"plan\": \"p1\"}]}\n```"
		case req.ReadOnly && strings.Contains(req.Message, "failed with exit code"):
			return "```grove-orchestrate\n{\"reply\": [{\"branch\": \"feature/c\", \"message\": \"The tests want c.txt to say blue.\"}]}\n```"
		case req.ReadOnly && strings.Contains(req.Message, "and it passed"):
			return "```grove-orchestrate\n{\"post\": [{\"plan\": \"p1\", \"message\": \"Merged and tested; have a look.\"}]}\n```"
		case req.Dir == cPath && strings.Contains(req.Message, "say blue"):
			os.WriteFile(filepath.Join(cPath, "c.txt"), []byte("blue\n"), 0o644)
			return "Done: c.txt says blue."
		}
		return ""
	}}
	m.runAgent = f.run
	planLog, _ := m.groupLog(p, plan.ChatID)
	chat := func() string {
		msgs, _ := planLog.All()
		var out []string
		for _, msg := range msgs {
			out = append(out, msg.From+": "+msg.Text)
		}
		return strings.Join(out, "\n")
	}

	m.finishIfSettled(p)
	settle(t, m, m.orchestrate(), func() bool {
		return strings.Contains(chat(), "Merged and tested") && !m.planner.running
	})
	if p.Status != plan.Review || p.Merge.Tests != plan.TestsPassed {
		t.Fatalf("plan = %s, tests %q", p.Status, p.Merge.Tests)
	}
	got := chat()
	for _, want := range []string{"Running the tests: `" + m.cfg.Planner.Test + "`", "failed with exit code 1. The planner is getting the tasks' agents to fix it", ") passed."} {
		if !strings.Contains(got, want) {
			t.Errorf("plan chat is missing %q:\n%s", want, got)
		}
	}
	// The planner heard about the failure with what to do, not the user.
	var failure string
	for _, r := range f.requests() {
		if r.ReadOnly && strings.Contains(r.Message, "failed with exit code") {
			failure = r.Message
			break
		}
	}
	if !strings.Contains(failure, "Don't ask the user yet") || !strings.Contains(failure, "Left out, as they failed: feature/a, feature/b") {
		t.Errorf("planner request about the failure:\n%s", failure)
	}
	if got := gitOut(t, p.Merge.Path, "show", "HEAD:c.txt"); got != "blue" {
		t.Errorf("merged c.txt = %q", got)
	}
}
