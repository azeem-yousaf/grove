package control

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormCommand(t *testing.T) {
	for _, c := range [][2]string{
		{"echo \\`date\\`  &&\tmake", "echo `date` && make"},
		{"git commit -m “Fix it”", `git commit -m "Fix it"`},
		{`grep \"x\" a`, `grep "x" a`},
	} {
		if got, want := NormCommand(c[0]), NormCommand(c[1]); got != want {
			t.Errorf("NormCommand(%q) = %q, want %q", c[0], got, want)
		}
	}
}

// TestAcknowledgements runs a stand-in for grove that carries out orders
// the way the UI reports them, and checks what the tools answer.
func TestAcknowledgements(t *testing.T) {
	dir := t.TempDir()
	if err := WriteState(dir, State{Plans: []Plan{{ID: "p1", Title: "Shop", Status: "running", Tasks: []Task{
		{Branch: "feature/api", Status: "working", Path: "/src/api", Approvals: []Approval{
			{ID: "ab12-1", Tool: "Bash", Command: "echo `date` && make", Planner: true},
			{ID: "ab12-2", Tool: "Bash", Command: "git push origin main", Reason: "pushes protected branch main"},
		}},
	}}}}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		done := map[string]bool{}
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			orders, _ := Take(dir)
			var rs []Result
			for _, o := range orders {
				r := Result{Order: o.ID, Outcome: Applied, Detail: "the agent was resumed with it"}
				switch {
				case o.Item == "ab12-2" && o.Action == Approve:
					r.Outcome, r.Detail = NeedsUser, "pushes protected branch main"
				case o.Item != "ab12-1" && o.Action != Message:
					r.Outcome, r.Detail = NotFound, "nothing waits on that"
				case done[o.Item+o.Action]:
					r.Outcome, r.Detail = AlreadyApplied, "it was already approved by the planner"
				}
				done[o.Item+o.Action] = true
				rs = append(rs, r)
			}
			Record(dir, rs)
		}
	}()

	for _, c := range []struct {
		tool   string
		args   map[string]any
		want   string
		failed bool
	}{
		{"approve", map[string]any{"id": "ab12-1"}, "Applied: the agent was resumed with it", false},
		// Matched by command, its backticks escaped and spaced differently.
		{"approve", map[string]any{"task": "feature/api", "command": "echo \\`date\\`   && make"}, "Already applied", false},
		{"approve", map[string]any{"task": "feature/api", "id": "ab12-2"}, "Needs the user: pushes protected branch main", true},
		{"deny", map[string]any{"task": "feature/api", "id": "zz-9"}, "Not found", true},
		{"message", map[string]any{"task": "feature/api", "text": "Go on."}, "Applied", false},
	} {
		got, failed := call(t, dir, c.tool, c.args)
		if failed != c.failed || !strings.HasPrefix(got, c.want) {
			t.Errorf("%s %v = %q (failed %v), want %q", c.tool, c.args, got, failed, c.want)
		}
	}
	close(stop)
	wg.Wait()
	if rs := readResults(dir); len(rs) != 5 {
		t.Errorf("results.jsonl holds %d results, want 5", len(rs))
	}
}

func TestResultsAreTrimmed(t *testing.T) {
	dir := t.TempDir()
	for i := range keepResults + 10 {
		if err := Record(dir, []Result{{Order: "o" + strconv.Itoa(i), Outcome: Applied}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(readResults(dir)); n != keepResults {
		t.Errorf("kept %d results", n)
	}
}

func TestLogActivity(t *testing.T) {
	dir := t.TempDir()
	if err := LogActivity(dir, []Activity{{ID: "ab12-1", Command: "npm test", Verdict: "approve", By: "planner", Outcome: Applied}}); err != nil {
		t.Fatal(err)
	}
	if err := LogActivity(dir, []Activity{{ID: "ab12-2", Command: "git push", Verdict: "approve", By: "grove-policy", Outcome: NeedsUser}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "approvals.jsonl"))
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); err != nil || len(lines) != 2 || !strings.Contains(lines[1], `"by":"grove-policy"`) {
		t.Errorf("approvals.jsonl = %q, %v", b, err)
	}
}

func call(t *testing.T, dir, tool string, args map[string]any) (string, bool) {
	t.Helper()
	b, _ := json.Marshal(args)
	for _, tl := range server(dir, Caller{}, "test").Tools {
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
			Approvals: []Approval{{ID: "ab12-1", Tool: "Bash", Command: "npm test", Planner: true, Stale: true}, {ID: "ab12-2", Tool: "Bash", Command: "git push origin main", Reason: "pushes protected branch main"}},
			LastReply: "Running the tests."},
		{Branch: "feature/ui", Title: "UI", Status: "queued"},
	}}}}); err != nil {
		t.Fatal(err)
	}

	got, failed := call(t, dir, "tasks", nil)
	for _, want := range []string{`Plan "Shop" (id p1, running)`, "Group chats: checkout", "- feature/api: API · working · agent working · worktree /src/api", "Waiting for approval, id ab12-1 (Bash, yours to answer): npm test (worktree changed since it was asked)",
		"Waiting for approval, id ab12-2 (Bash, only the user may answer: pushes protected branch main): git push origin main\n", "    Running the tests.", "- feature/ui: UI · queued"} {
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
		{"approve", map[string]any{"task": "feature/api"}, "Say which command"},
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

	// Nothing carries the orders out here, so the tools that wait say so.
	defer func(w time.Duration) { Wait = w }(Wait)
	Wait = 50 * time.Millisecond
	for _, ok := range []struct {
		tool string
		args map[string]any
	}{
		{"message", map[string]any{"task": "Feature/API", "text": "Use v2."}},
		{"deny", map[string]any{"task": "feature/api", "command": "npm  test", "reason": "use make test"}},
		{"mark", map[string]any{"task": "feature/ui", "status": "failed", "note": "not needed"}},
		{"post", map[string]any{"text": "Merged; try checkout."}},
		{"post", map[string]any{"plan": "shop", "group": "checkout", "to": "feature/ui", "text": "Wait for the API."}},
	} {
		got, failed := call(t, dir, ok.tool, ok.args)
		if failed {
			t.Errorf("%s: %s", ok.tool, got)
		}
		if (ok.tool == "message" || ok.tool == "deny") && !strings.Contains(got, "Still pending") {
			t.Errorf("%s without grove = %q", ok.tool, got)
		}
	}
	orders, err := Take(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Order{
		{Action: Message, Task: "feature/api", Text: "Use v2."},
		{Action: Deny, Task: "feature/api", Item: "ab12-1", Text: "npm test", Note: "use make test"},
		{Action: Fail, Task: "feature/ui", Note: "not needed"},
		{Action: Post, Plan: "p1", Text: "Merged; try checkout."},
		{Action: Post, Plan: "p1", Group: "checkout", To: "feature/ui", Text: "Wait for the API."},
	}
	if len(orders) != len(want) {
		t.Fatalf("orders = %+v", orders)
	}
	for i, o := range orders {
		if o.Time.IsZero() || o.ID == "" {
			t.Errorf("order %d has no time or ID", i)
		}
		if o.By != Lead {
			t.Errorf("order %d is by %q, want %q", i, o.By, Lead)
		}
		o.Time, o.ID, o.By = want[i].Time, "", ""
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

func callAs(t *testing.T, dir string, as Caller, tool string, args map[string]any) (string, bool) {
	t.Helper()
	b, _ := json.Marshal(args)
	for _, tl := range server(dir, as, "test").Tools {
		if tl.Name == tool {
			return tl.Call(b)
		}
	}
	t.Fatalf("no tool %s", tool)
	return "", true
}

// A group's planner sees and acts on its own group only, and its orders
// say who sent them.
func TestCrewTools(t *testing.T) {
	for _, s := range []string{"lead", "crew:p1:checkout", "crew:p1:a:b"} {
		c, ok := ParseCaller(s)
		if !ok || c.String() != s {
			t.Errorf("ParseCaller(%q) = %+v, %v", s, c, ok)
		}
	}
	for _, bad := range []string{"", "crew:", "crew:p1", "crew::g", "planner"} {
		if _, ok := ParseCaller(bad); ok {
			t.Errorf("ParseCaller(%q) should fail", bad)
		}
	}

	dir := t.TempDir()
	if err := WriteState(dir, State{Plans: []Plan{
		{ID: "p1", Title: "Shop", Status: "running", Groups: []string{"checkout", "search"}, Tasks: []Task{
			{Branch: "feature/cart", Group: "checkout", Status: "working", Path: "/src/cart", Approvals: []Approval{{ID: "c-1", Tool: "Bash", Command: "npm test", Planner: true}}},
			{Branch: "feature/find", Group: "search", Status: "working", Path: "/src/find", Approvals: []Approval{{ID: "f-1", Tool: "Bash", Command: "make", Planner: true}}},
			{Branch: "feature/docs", Status: "working", Path: "/src/docs"},
		}},
		{ID: "p2", Title: "Other", Status: "running", Tasks: []Task{{Branch: "feature/x", Status: "working", Path: "/src/x"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	crew := Caller{"p1", "checkout"}
	got, _ := callAs(t, dir, crew, "tasks", nil)
	if !strings.Contains(got, "feature/cart") || !strings.Contains(got, "Group chats: checkout\n") {
		t.Errorf("crew's tasks is missing its group:\n%s", got)
	}
	for _, other := range []string{"feature/find", "f-1", "feature/docs", "feature/x", "Other", "search"} {
		if strings.Contains(got, other) {
			t.Errorf("crew's tasks shows %q:\n%s", other, got)
		}
	}
	for _, bad := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"approve", map[string]any{"id": "f-1"}, "No task of your group"},
		{"deny", map[string]any{"task": "feature/find", "command": "make"}, "No task of your group"},
		{"message", map[string]any{"task": "feature/docs", "text": "hi"}, "No task of your group"},
		{"mark", map[string]any{"task": "feature/x", "status": "finished"}, "No task of your group"},
		{"post", map[string]any{"text": "hi", "group": "search"}, "has no group"},
		{"post", map[string]any{"text": "hi", "to": "feature/find"}, "has no task"},
		{"merge", nil, "lead planner merges"},
		{"close", nil, "lead planner closes"},
	} {
		if got, failed := callAs(t, dir, crew, bad.tool, bad.args); !failed || !strings.Contains(got, bad.want) {
			t.Errorf("crew %s %v = %q, want an error with %q", bad.tool, bad.args, got, bad.want)
		}
	}
	if orders, _ := Take(dir); len(orders) != 0 {
		t.Fatalf("refused orders were queued: %v", orders)
	}
	if got, failed := callAs(t, dir, crew, "post", map[string]any{"text": "hi"}); failed {
		t.Fatalf("crew post = %q", got)
	}
	if got, failed := callAs(t, dir, crew, "post", map[string]any{"text": "all", "group": "plan"}); failed {
		t.Fatalf("crew post in the plan chat = %q", got)
	}
	if got, failed := callAs(t, dir, crew, "mark", map[string]any{"task": "feature/cart", "status": "finished"}); failed {
		t.Fatalf("crew mark = %q", got)
	}
	orders, _ := Take(dir)
	if len(orders) != 3 || orders[0].Group != "checkout" || orders[1].Group != "plan" || orders[2].Task != "feature/cart" {
		t.Fatalf("orders = %+v", orders)
	}
	for _, o := range orders {
		if o.By != "crew:p1:checkout" {
			t.Errorf("order %+v doesn't say the crew sent it", o)
		}
	}
}
