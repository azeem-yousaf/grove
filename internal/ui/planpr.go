package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/gh"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/plan"
)

// Once a plan is merged into its own branch and its tests pass (or it has
// no test command), grove pushes that branch to origin and opens a pull
// request into the plan's base with the GitHub CLI ([planner]
// pull_requests). Merging again pushes again, which updates the same pull
// request. The base branch itself is never pushed.

// prHooks publish a merged plan; tests replace them.
type prHooks struct {
	push   func(ctx context.Context, dir, remote, branch string, env []string) error
	open   func(ctx context.Context, dir, branch string) (gh.PR, bool, error)
	create func(ctx context.Context, dir, base, head, title, body string) (string, error)
}

func defaultPRHooks() prHooks { return prHooks{push: git.Push, open: gh.Open, create: gh.Create} }

// planPRMsg is how publishing a plan went.
type planPRMsg struct {
	plan         string
	branch, base string
	url          string
	created      bool
	pushErr      error // the push failed, so gh wasn't tried
	ghErr        error
}

// prRemote is where plan branches are pushed.
const prRemote = "origin"

// localBranch is base without the remote DetectBase may have put in front.
func localBranch(base string) string {
	return strings.TrimPrefix(strings.TrimPrefix(base, "refs/heads/"), prRemote+"/")
}

// planBase is the branch p's pull request goes into.
func (m *Model) planBase(p *plan.Plan) string {
	if b := localBranch(p.Base); b != "" {
		return b
	}
	return localBranch(m.base)
}

// publishPlan pushes p's merge branch and opens its pull request, unless
// that is turned off. tests says how the tests went, for the description.
func (m *Model) publishPlan(p *plan.Plan, tests string) tea.Cmd {
	if !m.cfg.Planner.OpenPRs() || p.Merge == nil || p.Merge.Branch == "" || p.Merge.Path == "" {
		return nil
	}
	head, base := p.Merge.Branch, m.planBase(p)
	if base == "" || isProtectedBranch(head, m.protectedBranches()) {
		// Never push a protected branch, whatever the plan's branch is called.
		return m.grovePost(p, "grove didn't push "+head+": it is a protected branch, or the plan has no base branch to open a pull request into.")
	}
	body := m.prBody(p, tests)
	dir, id, title, env, hooks, hasGH := p.Merge.Path, p.ID, p.Title, m.guardEnv(), m.prHooks, m.ghPath != ""
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 3*time.Minute)
		defer cancel()
		out := planPRMsg{plan: id, branch: head, base: base}
		if err := hooks.push(ctx, dir, prRemote, head, env); err != nil {
			out.pushErr = err
			return out
		}
		if !hasGH {
			out.ghErr = fmt.Errorf("gh isn't installed")
			return out
		}
		if pr, ok, err := hooks.open(ctx, dir, head); err != nil {
			out.ghErr = err
			return out
		} else if ok {
			out.url = pr.URL
			return out
		}
		url, err := hooks.create(ctx, dir, base, head, title, body)
		out.url, out.created, out.ghErr = url, err == nil, err
		return out
	}
}

// prBody describes p's pull request: its tasks and how its tests went.
func (m *Model) prBody(p *plan.Plan, tests string) string {
	var b strings.Builder
	b.WriteString("Merged by grove from the plan \"" + p.Title + "\".\n\n## Tasks\n\n")
	for _, t := range p.Tasks {
		state := ""
		if t.Status == plan.Failed {
			state = " (failed, not merged)"
		}
		fmt.Fprintf(&b, "- %s (`%s`)%s\n", t.Title, t.Branch, state)
	}
	b.WriteString("\n## Tests\n\n")
	if test := m.testCommand(p); test != "" {
		fmt.Fprintf(&b, "`%s`: %s\n", test, tests)
	} else {
		b.WriteString("No test command was set.\n")
	}
	return b.String()
}

func (m *Model) onPlanPR(msg planPRMsg) tea.Cmd {
	p := m.planByID(msg.plan)
	if p == nil || p.Merge == nil {
		return nil
	}
	if msg.pushErr != nil || msg.ghErr != nil {
		what, err := "push "+msg.branch, msg.pushErr
		if err == nil {
			what, err = "open a pull request for "+msg.branch, msg.ghErr
		}
		text := fmt.Sprintf("grove couldn't %s: %v. To do it yourself, in %s:\n\n```\ngit push -u %s %s\ngh pr create --base %s --head %s --title %q\n```",
			what, err, p.Merge.Path, prRemote, msg.branch, msg.base, msg.branch, p.Title)
		m.plannerNote(roleError, "Couldn't "+what+": "+err.Error())
		return tea.Batch(m.grovePost(p, text), m.notify("Couldn't "+what+": "+err.Error(), toastErr), m.alert("Couldn't "+what))
	}
	p.Merge.PR = msg.url
	text := "Pushed " + msg.branch + "; its pull request " + msg.url + " has the new commits."
	if msg.created {
		text = "Opened a pull request from " + msg.branch + " into " + msg.base + ": " + msg.url
	}
	m.plannerNote(roleNote, text)
	cmds := []tea.Cmd{m.savePlan(p), m.grovePost(p, text)}
	if msg.created {
		cmds = append(cmds, m.notify("✦ "+p.Title+": pull request opened", toastOK), m.alert(p.Title+": pull request opened"))
	}
	return tea.Batch(cmds...)
}
