package ui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/gh"
	"github.com/azeem-yousaf/grove/internal/git"
)

// openShipFor opens the Ship menu on the selected worktree and answers its
// git lookups with up.
func openShipFor(t *testing.T, m *Model, up git.Upstream) {
	t.Helper()
	press(m, "s")
	if m.ship == nil {
		t.Fatal("s didn't open the Ship menu")
	}
	m.Update(shipLoadedMsg{path: m.ship.path, up: up, baseBranch: "main"})
}

func TestShipMenu(t *testing.T) {
	m := fakeModel(t, 140, 36) // feature/auth: 2 files not committed, 1 behind main
	openShipFor(t, m, git.Upstream{Remote: "origin"})
	out := plain(m)
	for _, want := range []string{
		"Ship feature/auth",
		"Commit", "2 files not committed yet",
		"Push", "first push, to origin/feature/auth",
		"Pull request", "install the GitHub CLI",
		"Merge main in", "1 commit behind",
		"Rebase onto main",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Ship menu is missing %q:\n%s", want, out)
		}
	}

	// An action that can't run says why and keeps the menu open.
	press(m, "r")
	if m.ship == nil || !strings.Contains(m.toast.text, "GitHub CLI") {
		t.Errorf("r without gh: ship=%v toast=%q", m.ship != nil, m.toast.text)
	}

	// Up to date with its upstream: nothing to push.
	m.Update(shipLoadedMsg{path: m.ship.path, up: git.Upstream{Name: "origin/feature/auth", Remote: "origin"}})
	if out := plain(m); !strings.Contains(out, "up to date with origin/feature/auth") {
		t.Errorf("push detail when up to date:\n%s", out)
	}
	press(m, "esc")
	if m.ship != nil {
		t.Error("esc didn't close the menu")
	}

	// The main worktree is the base: no merging it into itself.
	m = fakeModel(t, 140, 36)
	press(m, "[")
	openShipFor(t, m, git.Upstream{Name: "origin/main", Remote: "origin"})
	if out := plain(m); strings.Contains(out, "Merge main in") || !strings.Contains(out, "nothing to commit") {
		t.Errorf("Ship menu for main:\n%s", out)
	}
}

func TestCommitDialog(t *testing.T) {
	m := fakeModel(t, 140, 36)
	auth := filepath.FromSlash("/src/shop-auth")
	s := m.newSession(auth, "feature/auth", agent.Claude)
	s.entries = []chatEntry{{roleAgent, "Done.\n\n```grove-run\ngit add -A\ngit commit -am \"Add remember me\" -m \"Keeps the session 30 days.\"\n```"}}
	s.pending = []pendingAction{{kind: pendingCommand, text: `git commit -am "Add remember me"`}, {kind: pendingApproval, text: "npm test"}}

	openShipFor(t, m, git.Upstream{Remote: "origin"})
	press(m, "c")
	if m.ship != nil || m.commit == nil {
		t.Fatalf("c didn't open the commit dialog: ship=%v commit=%v", m.ship != nil, m.commit != nil)
	}
	c := m.commit
	if got := c.message.Value(); got != "Add remember me\n\nKeeps the session 30 days." {
		t.Errorf("message = %q", got)
	}
	var names []string
	for _, f := range c.files {
		names = append(names, f.f.Path)
	}
	if strings.Join(names, ",") != "src/auth/login.ts,src/auth/oauth.ts" {
		t.Errorf("files = %v", names)
	}
	out := plain(m)
	for _, want := range []string{"Commit feature/auth", "Files  2 of 2", "src/auth/oauth.ts"} {
		if !strings.Contains(out, want) {
			t.Errorf("commit dialog is missing %q:\n%s", want, out)
		}
	}

	// tab moves to the files; space unticks, a ticks them all again.
	press(m, "tab", "space")
	if c.files[0].on || !strings.Contains(plain(m), "Files  1 of 2") {
		t.Error("space didn't untick the first file")
	}
	press(m, "a")
	if !c.files[0].on || !c.files[1].on {
		t.Error("a didn't tick every file")
	}
	press(m, "space", "down", "space")
	press(m, "enter")
	if m.commit == nil || !strings.Contains(m.toast.text, "Choose at least one file") {
		t.Errorf("no file chosen: commit=%v toast=%q", m.commit != nil, m.toast.text)
	}

	// Without a message it doesn't commit either.
	press(m, "a", "tab")
	c.message.SetValue("  ")
	press(m, "enter")
	if m.commit == nil || !strings.Contains(m.toast.text, "message") {
		t.Errorf("empty message: commit=%v toast=%q", m.commit != nil, m.toast.text)
	}

	// The agent's suggested commit card goes once the work is committed.
	m.dropCommitCards(auth)
	if len(s.pending) != 1 || s.pending[0].text != "npm test" {
		t.Errorf("pending after the commit = %+v", s.pending)
	}
}

func TestCommitMessageOf(t *testing.T) {
	for line, want := range map[string]string{
		`git commit -m "Fix it"`:                "Fix it",
		`git commit -am 'Fix it' -m "Details."`: "Fix it\n\nDetails.",
		`git commit --message="Fix it"`:         "Fix it",
		`git commit --amend --no-edit`:          "",
		`git push`:                              "",
		`git commit -S -m "Signed"`:             "Signed",
	} {
		if got := commitMessageOf(line); got != want {
			t.Errorf("%s: %q, want %q", line, got, want)
		}
	}
}

func TestPullRequests(t *testing.T) {
	m := fakeModel(t, 140, 36)
	m.ghPath = "gh"
	calls := 0
	m.listPRs = func(context.Context, string) (map[string]gh.PR, error) {
		calls++
		return map[string]gh.PR{"feature/auth": {Number: 12, State: "OPEN", URL: "https://github.com/o/r/pull/12", Branch: "feature/auth"}}, nil
	}
	cmd := m.pollPRs(false)
	if cmd == nil {
		t.Fatal("no first poll")
	}
	m.Update(cmd())
	if m.pollPRs(false) != nil {
		t.Error("polled again within a minute")
	}
	out := plain(m)
	if !strings.Contains(out, "#12 open") {
		t.Errorf("list doesn't show the PR:\n%s", out)
	}
	openShipFor(t, m, git.Upstream{Name: "origin/feature/auth", Remote: "origin", Ahead: 1})
	if out := plain(m); !strings.Contains(out, "#12 open · open in the browser") || !strings.Contains(out, "1 commit not pushed yet") {
		t.Errorf("Ship menu with a PR:\n%s", out)
	}

	// gh failing (not signed in, not GitHub) stops polling and says why.
	m.listPRs = func(context.Context, string) (map[string]gh.PR, error) { return nil, errors.New("gh: not logged in") }
	m.prs, m.prPolling = nil, false // the poll the menu started never ran
	m.Update(m.pollPRs(true)())
	if !m.prOff || m.pollPRs(false) != nil {
		t.Error("kept polling after gh failed")
	}
	if out := plain(m); !strings.Contains(out, "not logged in") {
		t.Errorf("Ship menu doesn't say why there are no PRs:\n%s", out)
	}
}
