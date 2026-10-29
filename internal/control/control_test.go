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
	if err := WriteState(dir, State{Plans: []Plan{{ID: "p1", Title: "Shop", Status: "running", Groups: []string{"checkout"}, Tasks: []Task{
		{Branch: "feature/api", Title: "API", Status: "working", Path: "/src/api", Busy: true,
			Approvals: []Approval{{Tool: "Bash", Command: "npm test"}}, LastReply: "Running the tests."},
		{Branch: "feature/ui", Title: "UI", Status: "queued"},
	}}}}); err != nil {
		t.Fatal(err)
	}

	got, failed := call(t, dir, "tasks", nil)
	for _, want := range []string{`Plan "Shop" (id p1, running)`, "Group chats: checkout", "- feature/api: API · working · agent working · worktree /src/api", "Waiting for approval (Bash): npm test", "    Running the tests.", "- feature/ui: UI · queued"} {
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
		{"post", map[string]any{"text": "hi", "group": "nope"}, "has no group"},
		{"post", map[string]any{"text": "hi", "to": "feature/nope"}, "has no task"},
		{"post", map[string]any{"text": "hi", "plan": "Other"}, "No running plan"},
		{"post", map[string]any{"text": " "}, "empty"},
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
		{"post", map[string]any{"text": "Merged; try checkout."}},
		{"post", map[string]any{"plan": "shop", "group": "checkout", "to": "feature/ui", "text": "Wait for the API."}},
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
		{Action: Post, Plan: "p1", Text: "Merged; try checkout."},
		{Action: Post, Plan: "p1", Group: "checkout", To: "feature/ui", Text: "Wait for the API."},
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

func TestPlanTools(t *testing.T) {
	dir := t.TempDir()
	st := State{Plans: []Plan{{ID: "p1", Title: "Shop", Status: "running", Tasks: []Task{
		{Branch: "feature/a", Status: "finished"}, {Branch: "feature/b", Status: "working"},
	}}}}
	if err := WriteState(dir, st); err != nil {
		t.Fatal(err)
	}
	if got, failed := call(t, dir, "merge", nil); !failed || !strings.Contains(got, "feature/b is still working") {
		t.Errorf("merge while b works = %q", got)
	}
	if got, failed := call(t, dir, "close", map[string]any{"plan": "p1"}); !failed || !strings.Contains(got, "isn't merged") {
		t.Errorf("close before merging = %q", got)
	}
	st.Plans[0].Tasks[1].Status = "failed"
	if err := WriteState(dir, st); err != nil {
		t.Fatal(err)
	}
	if got, failed := call(t, dir, "merge", map[string]any{"plan": "Shop"}); failed {
		t.Errorf("merge = %q", got)
	}
	st.Plans[0].Status = "review"
	if err := WriteState(dir, st); err != nil {
		t.Fatal(err)
	}
	if got, failed := call(t, dir, "close", map[string]any{"note": "looks good"}); failed {
		t.Errorf("close = %q", got)
	}
	orders, _ := Take(dir)
	if len(orders) != 2 || orders[0].Action != Merge || orders[0].Plan != "p1" || orders[1].Action != Close || orders[1].Note != "looks good" {
		t.Errorf("orders = %+v", orders)
	}
}
