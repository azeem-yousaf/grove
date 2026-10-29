package ui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azeem-yousaf/grove/internal/agent"
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
	m.cfg.Planner.PullRequests = nil // on, as by default
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
	// Wait for the post too: the URL is saved before it is posted.
	settle(t, m, m.publishPlan(p, "passed"), func() bool {
		return p.Merge.PR != "" && strings.Contains(planChat(m, p), "Opened a pull request")
	})
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

// A failed push after the merge is grove's own news: the planner still
// gets the review ask and posts its summary.
func TestFailedPushKeepsThePlanReview(t *testing.T) {
	m, p, _ := groupedPlan(t)
	p.Orchestrated, p.Base = true, "main"
	for i := range p.Tasks {
		p.Tasks[i].Status = plan.Failed
		if p.Tasks[i].Branch == "feature/c" {
			p.Tasks[i].Status = plan.Finished
		}
	}
	m.cfg.Planner.PullRequests = nil
	m.ghPath = "gh"
	f := &fakePRs{open: map[string]string{}}
	m.prHooks = f.hooks()
	m.prHooks.push = func(context.Context, string, string, string, []string) error {
		return errors.New("remote rejected")
	}
	planner := &scripted{reply: func(req agent.Request) string {
		switch {
		case req.ReadOnly && strings.Contains(req.Message, "is finished or failed"):
			return "```grove-orchestrate\n{\"merge\": [{\"plan\": \"p1\"}]}\n```"
		case req.ReadOnly && strings.Contains(req.Message, "Merged 1 task"):
			return "```grove-orchestrate\n{\"post\": [{\"plan\": \"p1\", \"message\": \"All merged; try checkout.\"}]}\n```"
		}
		return ""
	}}
	m.runAgent = planner.run

	m.finishIfSettled(p)
	settle(t, m, m.orchestrate(), func() bool {
		c := planChat(m, p)
		return strings.Contains(c, "All merged; try checkout.") && strings.Contains(c, "couldn't push") && !m.planner.running
	})
	got := planChat(m, p)
	for _, want := range []string{"planner: All merged; try checkout.", "grove: grove couldn't push plan/shop: remote rejected"} {
		if n := strings.Count(got, want); n != 1 {
			t.Errorf("%q appears %d times in the plan chat:\n%s", want, n, got)
		}
	}
	if p.Status != plan.Review || p.Merge.PR != "" {
		t.Errorf("plan = %s, PR %q", p.Status, p.Merge.PR)
	}
	if _, created := f.counts(); created != 0 {
		t.Error("no pull request without a push")
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

func TestPlannerFollowsThePullRequest(t *testing.T) {
	m, p, f := mergedPlan(t)
	p.Merge.PR = "https://github.com/x/shop/pull/7"
	fake := &fakeAgent{}
	m.runAgent = fake.run
	var mu sync.Mutex
	review := gh.Review{State: "OPEN", Feedback: []gh.Feedback{
		{ID: "review:R1", Author: "ann", Body: "Rename total to sum.", State: "CHANGES_REQUESTED"},
		{ID: "line:9", Author: "ann", Body: "Off by one", Path: "c.txt", Line: 2},
	}}
	m.prHooks.review = func(context.Context, string, string) (gh.Review, error) {
		mu.Lock()
		defer mu.Unlock()
		return review, nil
	}
	_ = f
	poll := func() {
		for _, msg := range runCmds(m.pollReviews()) {
			_, cmd := m.Update(msg)
			pump(t, m, cmd)
		}
	}
	m.now = time.Now()
	poll()
	if len(fake.reqs) != 1 || !fake.reqs[0].ReadOnly {
		t.Fatalf("the planner should get the review: %d requests", len(fake.reqs))
	}
	ask := fake.reqs[0].Message
	for _, want := range []string{"ann (requested changes): Rename total to sum.", "ann on c.txt line 2: Off by one", "pull/7"} {
		if !strings.Contains(ask, want) {
			t.Errorf("planner request is missing %q:\n%s", want, ask)
		}
	}
	if !slices.Equal(p.Merge.Seen, []string{"review:R1", "line:9"}) {
		t.Errorf("seen = %q", p.Merge.Seen)
	}

	// Nothing new: no turn. Read again only after reviewEvery.
	if m.pollReviews() != nil {
		t.Error("polled again too soon")
	}
	m.now = m.now.Add(reviewEvery)
	poll()
	if len(fake.reqs) != 1 {
		t.Errorf("the same comments were passed on again: %d requests", len(fake.reqs))
	}

	// Once the user merges it, grove closes the plan.
	mu.Lock()
	review.State = "MERGED"
	mu.Unlock()
	m.now = m.now.Add(reviewEvery)
	poll()
	if p.Status != plan.Done {
		t.Errorf("status = %s, want done after the pull request was merged", p.Status)
	}
}
