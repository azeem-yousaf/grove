package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func call(t *testing.T, dir, tool string, args map[string]any) (string, bool) {
	t.Helper()
	b, _ := json.Marshal(args)
	for _, tl := range server(dir, "test").Tools {
		if tl.Name == tool {
			return tl.Call(b)
		}
	}
	t.Fatalf("no tool %s", tool)
	return "", true
}

func TestTools(t *testing.T) {
	dir := t.TempDir()
	if got, failed := call(t, dir, "tasks", nil); !failed || !strings.Contains(got, "Couldn't read") {
		t.Errorf("tasks before grove wrote anything = %q", got)
	}
	if err := WriteState(dir, State{Plans: []Plan{{Title: "Shop", Status: "running", Tasks: []Task{
		{Branch: "feature/api", Title: "API", Status: "working", Path: "/src/api", Busy: true,
			Approvals: []Approval{{Tool: "Bash", Command: "npm test"}}, LastReply: "Running the tests."},
		{Branch: "feature/ui", Title: "UI", Status: "queued"},
	}}}}); err != nil {
		t.Fatal(err)
	}

	got, failed := call(t, dir, "tasks", nil)
	for _, want := range []string{`Plan "Shop" (running)`, "- feature/api: API · working · agent working · worktree /src/api", "Waiting for approval (Bash): npm test", "    Running the tests.", "- feature/ui: UI · queued"} {
		if failed || !strings.Contains(got, want) {
			t.Errorf("tasks is missing %q:\n%s", want, got)
		}
	}

	for _, bad := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"message", map[string]any{"task": "feature/nope", "text": "hi"}, "No running task"},
		{"message", map[string]any{"task": "feature/ui", "text": "hi"}, "hasn't started yet"},
		{"message", map[string]any{"task": "feature/api", "text": " "}, "empty"},
		{"approve", map[string]any{"task": "feature/api", "command": "rm -rf /"}, "isn't waiting to run"},
		{"mark", map[string]any{"task": "feature/api", "status": "great"}, "status must be"},
	} {
		if got, failed := call(t, dir, bad.tool, bad.args); !failed || !strings.Contains(got, bad.want) {
			t.Errorf("%s %v = %q, want an error with %q", bad.tool, bad.args, got, bad.want)
		}
	}
	if orders, _ := Take(dir); len(orders) != 0 {
		t.Fatalf("refused orders were queued: %v", orders)
	}

	for _, ok := range []struct {
		tool string
		args map[string]any
	}{
		{"message", map[string]any{"task": "Feature/API", "text": "Use v2."}},
		{"deny", map[string]any{"task": "feature/api", "command": "npm test", "reason": "use make test"}},
		{"mark", map[string]any{"task": "feature/ui", "status": "failed", "note": "not needed"}},
	} {
		if got, failed := call(t, dir, ok.tool, ok.args); failed {
			t.Errorf("%s: %s", ok.tool, got)
		}
	}
	orders, err := Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Order{
		{Action: Message, Task: "feature/api", Text: "Use v2."},
		{Action: Deny, Task: "feature/api", Text: "npm test", Note: "use make test"},
		{Action: Fail, Task: "feature/ui", Note: "not needed"},
	}
	if len(orders) != len(want) {
		t.Fatalf("orders = %+v", orders)
	}
	for i, o := range orders {
		if o.Time.IsZero() {
			t.Errorf("order %d has no time", i)
		}
		o.Time = want[i].Time
		if o != want[i] {
			t.Errorf("order %d = %+v, want %+v", i, o, want[i])
		}
	}
	if orders, _ := Take(dir); len(orders) != 0 {
		t.Errorf("Take should empty the queue: %v", orders)
	}
}
