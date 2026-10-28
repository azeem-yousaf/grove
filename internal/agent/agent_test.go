package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake agent: with GROVE_FAKE_AGENT set it
// records its arguments and stdin, prints canned output and exits.
func TestMain(m *testing.M) {
	if kind := os.Getenv("GROVE_FAKE_AGENT"); kind != "" {
		switch kind {
		case "claude":
			fakeClaude()
		case "codex":
			in, _ := io.ReadAll(os.Stdin)
			os.WriteFile(os.Getenv("GROVE_FAKE_RECORD"), []byte(strings.Join(os.Args[1:], "\n")+"\n---\n"+string(in)), 0o644)
			fmt.Println(codexSample)
		case "broken":
			// Fails at startup without waiting for input, like a signed-out claude.
			fmt.Fprintln(os.Stderr, "Error: not logged in. Run claude to sign in.")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeClaude acts like `claude -p --input-format stream-json`: a turn per
// input line until stdin closes. A message of "wait" streams a little and
// then waits for an interrupt. Each start and message is appended to
// GROVE_FAKE_RECORD.
func fakeClaude() {
	rec, _ := os.OpenFile(os.Getenv("GROVE_FAKE_RECORD"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintf(rec, "start %s\n", strings.Join(os.Args[1:], " "))
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var in struct {
			Type    string `json:"type"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Request struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		json.Unmarshal(sc.Bytes(), &in)
		switch {
		case in.Type == "user":
			fmt.Fprintf(rec, "user %q\n", in.Message.Content)
			if strings.HasSuffix(in.Message.Content, "wait") {
				fmt.Println(`{"type":"system","subtype":"init","session_id":"s-123"}`)
				fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Thinking"}},"parent_tool_use_id":null}`)
				continue
			}
			fmt.Println(claudeSample)
		case in.Type == "control_request" && in.Request.Subtype == "interrupt":
			fmt.Fprintln(rec, "interrupt")
			fmt.Println(`{"type":"control_response","response":{"subtype":"success","request_id":"grove-stop"}}`)
			fmt.Println(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s-123"}`)
		}
	}
}

const claudeSample = `{"type":"system","subtype":"init","session_id":"s-123","cwd":"/w","tools":["Read","Edit"]}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Look"}},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ing…"}},"parent_tool_use_id":null}
{"type":"assistant","message":{"content":[{"type":"text","text":"Looking…"},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/w/a.go"}}]},"parent_tool_use_id":null}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"/w/a.go","old_string":"x","new_string":"y"}}]},"parent_tool_use_id":null}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t9","name":"Read","input":{"file_path":"/w/sub.go"}}]},"parent_tool_use_id":"t5"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]},"parent_tool_use_id":null}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"npm test"}}]},"parent_tool_use_id":null}
{"type":"assistant","message":{"content":[{"type":"text","text":"Done. Run the tests to check."}]},"parent_tool_use_id":null}
{"type":"result","subtype":"success","is_error":false,"result":"Done. Run the tests to check.","session_id":"s-123","total_cost_usd":0.0421,"permission_denials":[{"tool_name":"Bash","tool_use_id":"t3","tool_input":{"command":"npm test"}}]}`

const codexSample = `{"type":"thread.started","thread_id":"th-9"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"bash -lc ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"bash -lc ls","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"i2","type":"file_change","changes":[{"path":"src/a.ts","kind":"update"},{"path":"src/b.ts","kind":"add"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"i3","item_type":"agent_message","text":"Updated a.ts and added b.ts."}}
{"type":"turn.completed","usage":{"input_tokens":12000,"cached_input_tokens":0,"output_tokens":340}}`

func feed(p parser, sample string) []Event {
	var evs []Event
	for _, l := range strings.Split(sample, "\n") {
		evs = append(evs, p.line([]byte(l))...)
	}
	return append(evs, p.done(nil, ""))
}

func TestClaudeParser(t *testing.T) {
	evs := feed(&claudeParser{}, claudeSample)
	var deltas, texts, tools []string
	var done Event
	for _, ev := range evs {
		switch ev.Kind {
		case EvDelta:
			deltas = append(deltas, ev.Text)
		case EvText:
			texts = append(texts, ev.Text)
		case EvTool:
			tools = append(tools, ev.Tool+" "+ev.Detail+" "+ev.Path)
		case EvDone:
			done = ev
		}
	}
	if strings.Join(deltas, "") != "Looking…" {
		t.Errorf("deltas = %q", deltas)
	}
	if len(texts) != 2 || texts[1] != "Done. Run the tests to check." {
		t.Errorf("texts = %q (the result must not repeat the last message)", texts)
	}
	wantTools := []string{"Read /w/a.go ", "Edit /w/a.go /w/a.go", "Bash npm test "}
	if strings.Join(tools, "|") != strings.Join(wantTools, "|") {
		t.Errorf("tools = %q, want %q (subagent steps skipped)", tools, wantTools)
	}
	if done.Session != "s-123" || done.Cost != "$0.04" || done.Err != nil {
		t.Errorf("done = %+v", done)
	}
	if len(done.Denied) != 1 || done.Denied[0].Tool != "Bash" || done.Denied[0].Detail != "npm test" {
		t.Errorf("denied = %+v", done.Denied)
	}

	// An error result carries its message.
	p := &claudeParser{}
	p.line([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"],"session_id":"s"}`))
	if ev := p.done(nil, ""); ev.Err == nil || ev.Err.Error() != "boom" {
		t.Errorf("error result = %v", ev.Err)
	}
	// No result at all: explain from stderr.
	if ev := (&claudeParser{}).done(fmt.Errorf("exit status 1"), "warn\nError: not logged in\n"); ev.Err == nil || !strings.Contains(ev.Err.Error(), "not logged in") {
		t.Errorf("missing result = %v", ev.Err)
	}
}

func TestCodexParser(t *testing.T) {
	evs := feed(&codexParser{}, codexSample)
	var got []string
	var done Event
	for _, ev := range evs {
		switch ev.Kind {
		case EvSession:
			got = append(got, "session "+ev.Session)
		case EvText:
			got = append(got, "text "+ev.Text)
		case EvTool:
			got = append(got, "tool "+ev.Tool+" "+ev.Detail+" "+ev.Path)
		case EvDone:
			done = ev
		}
	}
	want := []string{"session th-9", "tool Run bash -lc ls ", "tool Edit src/a.ts src/a.ts", "tool Edit src/b.ts src/b.ts", "text Updated a.ts and added b.ts."}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("events =\n%q\nwant\n%q", got, want)
	}
	if done.Err != nil || done.Cost != "12.3k tokens" {
		t.Errorf("done = %+v", done)
	}

	p := &codexParser{}
	p.line([]byte(`{"type":"turn.failed","error":{"message":"rate limited"}}`))
	if ev := p.done(nil, ""); ev.Err == nil || ev.Err.Error() != "rate limited" {
		t.Errorf("turn.failed = %v", ev.Err)
	}
}

func TestArgs(t *testing.T) {
	req := Request{Message: "fix it", Context: "CTX", Session: "s1", Allow: []string{ClaudeRule("Bash", "npm test")}, Model: "sonnet"}
	args := claudeArgs(req)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--input-format stream-json", "--output-format stream-json", "--verbose", "--permission-mode acceptEdits", "--resume s1", "--model sonnet", "Bash(git diff *)", "PowerShell(git status)", "PowerShell(git log *)", "Bash(npm test)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude args missing %q: %s", want, joined)
		}
	}
	if args[len(args)-1] != "Bash(npm test)" {
		t.Error("--allowedTools values must come last")
	}
	var msg struct {
		Type    string
		Message struct{ Role, Content string }
	}
	line := claudeInput(req)
	if err := json.Unmarshal(line, &msg); err != nil || line[len(line)-1] != '\n' {
		t.Fatalf("claude input isn't one JSON line: %q", line)
	}
	if msg.Type != "user" || msg.Message.Role != "user" || !strings.HasPrefix(msg.Message.Content, "CTX") || !strings.HasSuffix(msg.Message.Content, "## Request\n\nfix it") {
		t.Errorf("claude input = %+v", msg)
	}

	args, in := codexArgs(Request{Message: "and now?", Context: "CTX", History: []Turn{{User: "hi", Agent: "hello"}}})
	if strings.Join(args, " ") != "exec --json --sandbox workspace-write -" {
		t.Errorf("codex args = %q", args)
	}
	if !strings.Contains(in, "User: hi\n\nYou: hello") || !strings.HasSuffix(in, "and now?") {
		t.Errorf("codex input = %q", in)
	}
}

func TestReadOnlyArgs(t *testing.T) {
	req := Request{Dir: "/src/app", ReadOnly: true, Allow: []string{ClaudeRule("Bash", "ls")}}
	args := claudeArgs(req)
	joined := strings.Join(args, " ")
	for _, want := range []string{"--permission-mode default", "--disallowedTools Edit MultiEdit Write NotebookEdit --allowedTools", "mcp__claude_ai_Atlassian__getJiraIssue", "Bash(git log *)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("read-only claude args missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "acceptEdits") || strings.Contains(joined, "createJiraIssue") {
		t.Errorf("read-only claude args allow too much: %s", joined)
	}
	if args[len(args)-1] != "Bash(ls)" {
		t.Error("--allowedTools values must come last")
	}
	if args, _ := codexArgs(req); strings.Join(args, " ") != "exec --json --sandbox read-only -" {
		t.Errorf("read-only codex args = %q", args)
	}
	tools := []ToolServer{{Name: "grove", Command: `C:\bin\grove.exe`, Args: []string{"mcp-group", "--as", "feature/x"}, Allow: []string{"post", "read"}}}
	args = claudeArgs(Request{Tools: tools, Allow: []string{"Bash(ls)"}})
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, `--mcp-config {"mcpServers":{"grove":{"args":["mcp-group","--as","feature/x"],"command":"C:\\bin\\grove.exe","type":"stdio"}}}`) ||
		!strings.Contains(joined, "mcp__grove__post mcp__grove__read") || args[len(args)-1] != "Bash(ls)" {
		t.Errorf("claude args with tools: %s", joined)
	}
	if strings.Contains(strings.Join(claudeArgs(Request{}), " "), "--mcp-config") {
		t.Error("no tools should mean no --mcp-config")
	}
	cargs, _ := codexArgs(Request{Tools: tools})
	if j := strings.Join(cargs, " "); !strings.Contains(j, `-c mcp_servers.grove.command="C:\\bin\\grove.exe" -c mcp_servers.grove.args=["mcp-group","--as","feature/x"]`) {
		t.Errorf("codex args with tools: %s", j)
	}
	// The planner and the worktree's own agent never share a process.
	if procKey(req) == procKey(Request{Dir: "/src/app"}) {
		t.Error("read-only and normal agents in one folder share a process key")
	}
}

// turn runs one turn and returns its events, checking it ends with exactly
// one EvDone.
func turn(t *testing.T, r *Runner, ctx context.Context, req Request) []Event {
	t.Helper()
	var evs []Event
	r.Run(ctx, req, func(ev Event) { evs = append(evs, ev) })
	for i, ev := range evs {
		if (ev.Kind == EvDone) != (i == len(evs)-1) {
			t.Fatalf("a turn must end with exactly one EvDone: %+v", evs)
		}
	}
	return evs
}

// record is what the fake agents wrote to GROVE_FAKE_RECORD.
func record(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(os.Getenv("GROVE_FAKE_RECORD"))
	return string(b)
}

func fakeAgent(t *testing.T, kind string) (bin, dir string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	t.Setenv("GROVE_FAKE_AGENT", kind)
	t.Setenv("GROVE_FAKE_RECORD", filepath.Join(dir, "record.txt"))
	return self, dir
}

func TestClaudeKeepsItsProcess(t *testing.T) {
	bin, dir := fakeAgent(t, "claude")
	r := NewRunner()
	defer r.Stop(dir)
	ctx := context.Background()
	req := Request{Kind: Claude, Bin: bin, Dir: dir, Message: "hello there", Context: "CTX"}

	evs := turn(t, r, ctx, req)
	done := evs[len(evs)-1]
	if done.Err != nil || done.Session != "s-123" {
		t.Fatalf("first turn done = %+v", done)
	}
	// The next message goes to the same process.
	req.Message, req.Context, req.Session = "and again", "", done.Session
	if evs := turn(t, r, ctx, req); evs[len(evs)-1].Err != nil {
		t.Fatalf("second turn = %+v", evs)
	}
	rec := record(t)
	if strings.Count(rec, "start ") != 1 {
		t.Fatalf("both turns should share one process:\n%s", rec)
	}
	if !strings.Contains(rec, `user "CTX\n\n## Request\n\nhello there"`) || !strings.Contains(rec, `user "## Request\n\nand again"`) {
		t.Errorf("messages not delivered:\n%s", rec)
	}

	// A newly approved command needs a process that allows it; it picks up
	// the conversation.
	req.Message, req.Allow = "go on", []string{ClaudeRule("Bash", "npm test")}
	turn(t, r, ctx, req)
	rec = record(t)
	last := rec[strings.LastIndex(rec, "start "):]
	last = last[:strings.IndexByte(last, '\n')]
	if strings.Count(rec, "start ") != 2 || !strings.Contains(last, "--resume s-123") || !strings.HasSuffix(last, "Bash(npm test)") {
		t.Errorf("approving should restart with --resume and the rule:\n%s", rec)
	}
}

func TestClaudeStopKeepsProcess(t *testing.T) {
	bin, dir := fakeAgent(t, "claude")
	r := NewRunner()
	defer r.Stop(dir)
	ctx, cancel := context.WithCancel(context.Background())
	var evs []Event
	go func() {
		// Stop once the reply has started.
		for {
			time.Sleep(10 * time.Millisecond)
			if strings.Contains(record(t), "wait") {
				cancel()
				return
			}
		}
	}()
	r.Run(ctx, Request{Kind: Claude, Bin: bin, Dir: dir, Message: "wait"}, func(ev Event) { evs = append(evs, ev) })
	done := evs[len(evs)-1]
	if done.Kind != EvDone || !errors.Is(done.Err, errStopped) || done.Session != "s-123" {
		t.Fatalf("stopped turn done = %+v", done)
	}
	if !strings.Contains(record(t), "interrupt") {
		t.Error("stopping should interrupt the turn, not kill the process")
	}
	evs = turn(t, r, context.Background(), Request{Kind: Claude, Bin: bin, Dir: dir, Message: "next", Session: "s-123"})
	if evs[len(evs)-1].Err != nil || strings.Count(record(t), "start ") != 1 {
		t.Errorf("the process should still be usable after a stop:\n%s", record(t))
	}
}

func TestClaudeWarmIdleAndStop(t *testing.T) {
	bin, dir := fakeAgent(t, "claude")
	r := NewRunner()
	req := Request{Kind: Claude, Bin: bin, Dir: dir}
	r.Warm(req)
	r.Warm(req)
	req.Message = "hi"
	turn(t, r, context.Background(), req)
	if n := strings.Count(record(t), "start "); n != 1 {
		t.Fatalf("a turn should use the warmed process; started %d", n)
	}

	r.mu.Lock()
	p := r.procs[dir]
	r.mu.Unlock()
	r.Stop(dir)
	if p == nil || !p.ended() {
		t.Fatal("Stop should wait for the process to exit")
	}

	// An idle process ends by itself.
	r.Idle = 20 * time.Millisecond
	r.Warm(Request{Kind: Claude, Bin: bin, Dir: dir})
	r.mu.Lock()
	p = r.procs[dir]
	r.mu.Unlock()
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("an idle process should end")
	}
	// Codex has nothing to warm.
	r.Warm(Request{Kind: Codex, Bin: bin, Dir: dir})
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.procs) != 0 {
		t.Error("an ended process should be forgotten, and Codex not warmed")
	}
}

func TestCodexRunsOnce(t *testing.T) {
	bin, dir := fakeAgent(t, "codex")
	evs := turn(t, NewRunner(), context.Background(), Request{Kind: Codex, Bin: bin, Dir: dir, Message: "hello there", Context: "CTX"})
	if done := evs[len(evs)-1]; done.Err != nil {
		t.Fatalf("codex done = %+v", done)
	}
	if rec := record(t); !strings.Contains(rec, "---\nCTX") || !strings.HasSuffix(rec, "hello there") {
		t.Errorf("stdin not delivered: %q", rec)
	}
}

func TestAgentErrors(t *testing.T) {
	bin, dir := fakeAgent(t, "broken")
	r := NewRunner()
	evs := turn(t, r, context.Background(), Request{Kind: Claude, Bin: bin, Dir: dir, Message: "x"})
	if last := evs[len(evs)-1]; last.Err == nil || !strings.Contains(last.Err.Error(), "not logged in") {
		t.Errorf("broken agent = %+v", last)
	}
	if len(r.procs) != 0 {
		t.Error("a process that exited should be forgotten")
	}

	old := lookPath
	defer func() { lookPath = old }()
	lookPath = func(string) (string, error) { return "", fmt.Errorf("not found") }
	if evs := turn(t, r, context.Background(), Request{Kind: Claude, Dir: dir}); evs[0].Err == nil || !strings.Contains(evs[0].Err.Error(), "isn't installed") {
		t.Errorf("a missing agent should say so: %+v", evs)
	}
}

func TestFindPrefersExe(t *testing.T) {
	old := lookPath
	defer func() { lookPath = old }()
	lookPath = func(name string) (string, error) {
		switch name {
		case "claude", "claude.exe", "codex":
			return "/bin/" + name, nil
		}
		return "", fmt.Errorf("not found")
	}
	if got := Installed(); len(got) != 2 {
		t.Errorf("installed = %v", got)
	}
	if k, ok := Parse(" Codex "); !ok || k != Codex {
		t.Error("Parse should ignore case and spaces")
	}
}
