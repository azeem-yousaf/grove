package ui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/azeem-yousaf/grove/internal/gh"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/shell"
)

// fakePRs records pushes and pull requests instead of making them.
type fakePRs struct {
	mu      sync.Mutex
	pushed  []string
	created []string
	open    map[string]string // branch → URL of its open PR
	fail    error             // what gh pr create says
}

func (f *fakePRs) hooks() prHooks {
	return prHooks{
		push: func(_ context.Context, _, remote, branch string, _ []string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.pushed = append(f.pushed, remote+" "+branch)
			return nil
		},
		open: func(_ context.Context, _, branch string) (gh.PR, bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			url, ok := f.open[branch]
			return gh.PR{URL: url, Branch: branch, State: "OPEN"}, ok, nil
		},
		create: func(_ context.Context, _, base, head, title, body string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.fail != nil {
				return "", f.fail
			}
			f.created = append(f.created, base+" <- "+head+": "+title+"\n"+body)
			url := "https://github.com/x/shop/pull/7"
			f.open[head] = url
			return url, nil
		},
	}
}

func (f *fakePRs) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pushed), len(f.created)
}

// mergedPlan is groupedPlan merged into plan/shop, for publishing.
func mergedPlan(t *testing.T) (*Model, *plan.Plan, *fakePRs) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated, p.Base, p.Status = true, "main", plan.Review
	p.Merge = &plan.Merge{Branch: "plan/shop", Path: m.taskPath(p, "feature/c")}
	m.ghPath = "gh"
	f := &fakePRs{open: map[string]string{}}
	m.prHooks = f.hooks()
	return m, p, f
}

func planChat(m *Model, p *plan.Plan) string {
	log, _ := m.groupLog(p, plan.ChatID)
	msgs, _ := log.All()
	var out []string
	for _, msg := range msgs {
		out = append(out, msg.From+": "+msg.Text)
	}
	return strings.Join(out, "\n")
}

func TestMergedPlanGetsOnePullRequest(t *testing.T) {
	m, p, f := mergedPlan(t)
	m.cfg.Planner.Test = "go test ./..."
	settle(t, m, m.publishPlan(p, "passed"), func() bool { return p.Merge.PR != "" })
	if pushes, created := f.counts(); pushes != 1 || created != 1 {
		t.Fatalf("pushes %d, created %d", pushes, created)
	}
	if f.pushed[0] != "origin plan/shop" {
		t.Errorf("pushed %q", f.pushed[0])
	}
	pr := f.created[0]
	for _, want := range []string{"main <- plan/shop: Shop", "Work on feature/a (`feature/a`)", "`go test ./...`: passed"} {
		if !strings.Contains(pr, want) {
			t.Errorf("pull request is missing %q:\n%s", want, pr)
		}
	}
	if !strings.Contains(planChat(m, p), "grove: Opened a pull request from plan/shop into main: https://github.com/x/shop/pull/7") {
		t.Errorf("plan chat:\n%s", planChat(m, p))
	}

	// Merging again pushes again; the same pull request carries it.
	settle(t, m, m.publishPlan(p, "passed"), func() bool {
		return strings.Contains(planChat(m, p), "has the new commits")
	})
	if pushes, created := f.counts(); pushes != 2 || created != 1 {
		t.Errorf("after merging again: pushes %d, created %d", pushes, created)
	}
}

func TestNoPullRequestWhenTestsFail(t *testing.T) {
	m, p, f := mergedPlan(t)
	m.testingPlan[p.ID] = true
	settle(t, m, m.onTestsDone(testsDoneMsg{plan: p.ID, command: "go test ./...", res: shell.Result{ExitCode: 1}}), func() bool {
		return strings.Contains(planChat(m, p), "The tests (`go test ./...`) failed")
	})
	if pushes, created := f.counts(); pushes != 0 || created != 0 {
		t.Errorf("failed tests: pushes %d, created %d", pushes, created)
	}
}

func TestPullRequestFailureTellsTheUserWhatToRun(t *testing.T) {
	m, p, f := mergedPlan(t)
	f.fail = errors.New("gh pr create: not logged in")
	settle(t, m, m.publishPlan(p, ""), func() bool { return strings.Contains(planChat(m, p), "couldn't open a pull request") })
	got := planChat(m, p)
	for _, want := range []string{"not logged in", "git push -u origin plan/shop", "gh pr create --base main --head plan/shop --title \"Shop\""} {
		if !strings.Contains(got, want) {
			t.Errorf("plan chat is missing %q:\n%s", want, got)
		}
	}
	if p.Merge.PR != "" {
		t.Error("no PR to remember")
	}
}

func TestBaseBranchIsNeverPushed(t *testing.T) {
	m, p, f := mergedPlan(t)
	p.Merge.Branch = "main"
	settle(t, m, m.publishPlan(p, "passed"), func() bool { return strings.Contains(planChat(m, p), "didn't push main") })
	if pushes, _ := f.counts(); pushes != 0 {
		t.Error("a protected branch was pushed")
	}
	off := false
	m.cfg.Planner.PullRequests = &off
	p.Merge.Branch = "plan/shop"
	if m.publishPlan(p, "passed") != nil {
		t.Error("pull_requests = false should do nothing")
	}
}
