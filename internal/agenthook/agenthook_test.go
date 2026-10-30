package agenthook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azeem-yousaf/grove/internal/gitpolicy"
)

func TestPolicyRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "control")
	if _, err := ReadPolicy(dir); err == nil {
		t.Fatal("a missing policy should be an error")
	}
	var p Policy
	wt := filepath.Join(t.TempDir(), "app-feature-x")
	p.Set(AgentScope{Scope: gitpolicy.Scope{Worktree: wt, Branch: "feature/x", Protected: []string{"main"}, Guarded: true}, Planner: true})
	if err := WritePolicy(dir, p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := got.Scope(wt + string(filepath.Separator))
	if got.Version != PolicyVersion || !ok || s.Branch != "feature/x" || !s.Guarded || !s.Planner || s.Protected[0] != "main" {
		t.Errorf("policy = %+v, scope = %+v", got, s)
	}
	if _, ok := got.Scope(filepath.Join(wt, "other")); ok {
		t.Error("an unknown worktree has no scope")
	}
}

func TestGrantUsedOnce(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "wt")
	if _, ok := TakeGrant(dir, wt, "git push"); ok {
		t.Fatal("nothing granted yet")
	}
	if err := AddGrant(dir, Grant{Worktree: wt, Command: "git  push origin\tmain"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := TakeGrant(dir, filepath.Join(dir, "other"), "git push origin main"); ok {
		t.Error("a grant is for its own worktree")
	}
	g, ok := TakeGrant(dir, wt, " git push origin main ")
	if !ok || g.ID == "" || g.Time.IsZero() {
		t.Fatalf("grant not taken: %+v %v", g, ok)
	}
	if _, ok := TakeGrant(dir, wt, "git push origin main"); ok {
		t.Error("a grant is used once")
	}
	// Old grants expire.
	AddGrant(dir, Grant{Worktree: wt, Command: "git push", Time: time.Now().Add(-2 * GrantTTL)})
	if _, ok := TakeGrant(dir, wt, "git push"); ok {
		t.Error("an expired grant was used")
	}
}

func TestConcurrentGrants(t *testing.T) {
	dir := t.TempDir()
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := AddGrant(dir, Grant{Worktree: dir, Command: fmt.Sprintf("git push %d", i)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var taken sync.Map
	for i := range n {
		wg.Add(2)
		for range 2 { // two takers race for each grant
			go func() {
				defer wg.Done()
				if _, ok := TakeGrant(dir, dir, fmt.Sprintf("git push %d", i)); ok {
					if _, dup := taken.LoadOrStore(i, true); dup {
						t.Errorf("grant %d used twice", i)
					}
				}
			}()
		}
	}
	wg.Wait()
	for i := range n {
		if _, ok := taken.Load(i); !ok {
			t.Errorf("grant %d lost", i)
		}
	}
}

// fakeLookup is a repository on feature/x where nothing is pushed.
type fakeLookup struct{}

func (fakeLookup) CurrentBranch(string) (string, error) { return "feature/x", nil }
func (fakeLookup) Alias(string) (string, bool)          { return "", false }
func (fakeLookup) RefExists(string) bool                { return true }
func (fakeLookup) Pushed(string) bool                   { return false }

func hookRun(t *testing.T, h Hook, input string) (decision, reason string) {
	t.Helper()
	h.Lookup = func(context.Context, string) gitpolicy.Lookup { return fakeLookup{} }
	var out bytes.Buffer
	if err := h.Run(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		return "", ""
	}
	var o struct {
		HookSpecificOutput struct {
			HookEventName, PermissionDecision, PermissionDecisionReason string
		}
	}
	if err := json.Unmarshal(out.Bytes(), &o); err != nil || o.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("hook output %q: %v", out.String(), err)
	}
	return o.HookSpecificOutput.PermissionDecision, o.HookSpecificOutput.PermissionDecisionReason
}

func call(tool, cwd, cmd string) string {
	b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": tool, "cwd": cwd, "tool_input": map[string]any{"command": cmd}})
	return string(b)
}

func TestHookDecisions(t *testing.T) {
	dir := t.TempDir()
	wt := filepath.Join(dir, "wt")
	h := Hook{Dir: dir, Worktree: wt}

	// No policy yet: git asks (fail closed), anything else is left alone.
	if d, _ := hookRun(t, h, call("Bash", wt, "git status")); d != "ask" {
		t.Errorf("git without a policy = %q, want ask", d)
	}
	if d, _ := hookRun(t, h, call("Bash", wt, "ls -la")); d != "" {
		t.Errorf("non-git without a policy = %q, want nothing", d)
	}

	var p Policy
	p.Set(AgentScope{Scope: gitpolicy.Scope{Worktree: wt, Branch: "feature/x", Protected: []string{"main"}}})
	if err := WritePolicy(dir, p); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tool, cmd, want string }{
		{"Bash", "ls -la", ""},
		{"Bash", "git status", "allow"},
		{"PowerShell", "git log -3", "allow"},
		{"Bash", "git push origin HEAD:main", "ask"},
		{"Read", "git push origin HEAD:main", ""}, // not a shell
	} {
		d, reason := hookRun(t, h, call(c.tool, wt, c.cmd))
		if d != c.want {
			t.Errorf("%s %q = %q (%s), want %q", c.tool, c.cmd, d, reason, c.want)
		}
		if d == "ask" && reason == "" {
			t.Errorf("%q asks without a reason", c.cmd)
		}
	}

	// Claude's prefix rules (the plan's checks, the planner's family
	// approvals, such as Bash(go test:*)) never cover git: the hook still
	// judges git chained onto an allowed family.
	for _, c := range []struct{ cmd, want string }{
		{"go test ./... -run X", ""},
		{"go test ./... && git push origin HEAD:main", "ask"},
		{"go test ./...; git push --force origin feature/x", "ask"},
		{"go vet ./... | git update-ref refs/heads/main HEAD", "ask"},
	} {
		if d, reason := hookRun(t, h, call("Bash", wt, c.cmd)); d != c.want {
			t.Errorf("%q = %q (%s), want %q", c.cmd, d, reason, c.want)
		}
	}

	// An approved command runs once.
	AddGrant(dir, Grant{Worktree: wt, Command: "git push origin HEAD:main"})
	if d, _ := hookRun(t, h, call("Bash", wt, "git push origin HEAD:main")); d != "allow" {
		t.Errorf("granted command = %q, want allow", d)
	}
	if d, _ := hookRun(t, h, call("Bash", wt, "git push origin HEAD:main")); d != "ask" {
		t.Errorf("used grant = %q, want ask", d)
	}

	// Another worktree has no scope, so its git asks.
	if d, _ := hookRun(t, Hook{Dir: dir, Worktree: filepath.Join(dir, "other")}, call("Bash", wt, "git status")); d != "ask" {
		t.Errorf("unknown worktree = %q, want ask", d)
	}
	// So does anything when the input can't be read.
	if d, _ := hookRun(t, h, "{not json"); d != "ask" {
		t.Errorf("malformed input = %q, want ask", d)
	}
	// A damaged policy fails closed too.
	os.WriteFile(filepath.Join(dir, "policy.json"), []byte("{"), 0o644)
	if d, _ := hookRun(t, h, call("Bash", wt, "git status")); d != "ask" {
		t.Errorf("damaged policy = %q, want ask", d)
	}
}
