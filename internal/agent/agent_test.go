package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// The test binary doubles as a fake agent: with GROVE_FAKE_AGENT set it
// records its arguments and stdin, prints canned output and exits.
func TestMain(m *testing.M) {
	if kind := os.Getenv("GROVE_FAKE_AGENT"); kind != "" {
		in, _ := io.ReadAll(os.Stdin)
		if rec := os.Getenv("GROVE_FAKE_RECORD"); rec != "" {
			os.WriteFile(rec, []byte(strings.Join(os.Args[1:], "\n")+"\n---\n"+string(in)), 0o644)
		}
		switch kind {
		case "claude":
			fmt.Println(claudeSample)
		case "codex":
			fmt.Println(codexSample)
		case "broken":
			fmt.Fprintln(os.Stderr, "Error: not logged in. Run claude to sign in.")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
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
	args, in := claudeArgs(Request{Message: "fix it", Context: "CTX", Session: "s1", Allow: []string{ClaudeRule("npm test")}, Model: "sonnet"})
	joined := strings.Join(args, " ")
	for _, want := range []string{"--output-format stream-json", "--verbose", "--permission-mode acceptEdits", "--resume s1", "--model sonnet", "Bash(git diff *)", "Bash(npm test)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("claude args missing %q: %s", want, joined)
		}
	}
	if args[len(args)-1] != "Bash(npm test)" {
		t.Error("--allowedTools values must come last")
	}
	if !strings.HasPrefix(in, "CTX") || !strings.HasSuffix(in, "## Request\n\nfix it") {
		t.Errorf("claude input = %q", in)
	}

	args, in = codexArgs(Request{Message: "and now?", Context: "CTX", History: []Turn{{User: "hi", Agent: "hello"}}})
	if strings.Join(args, " ") != "exec --json --sandbox workspace-write -" {
		t.Errorf("codex args = %q", args)
	}
	if !strings.Contains(in, "User: hi\n\nYou: hello") || !strings.HasSuffix(in, "and now?") {
		t.Errorf("codex input = %q", in)
	}
}

func TestRunWithFakeAgents(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, kind := range []Kind{Claude, Codex} {
		rec := dir + "/" + string(kind) + ".txt"
		t.Setenv("GROVE_FAKE_AGENT", string(kind))
		t.Setenv("GROVE_FAKE_RECORD", rec)
		var evs []Event
		Run(context.Background(), Request{Kind: kind, Bin: self, Dir: dir, Message: "hello there", Context: "CTX"}, func(ev Event) {
			evs = append(evs, ev)
		})
		last := evs[len(evs)-1]
		if last.Kind != EvDone || last.Err != nil {
			t.Fatalf("%s: last event = %+v", kind, last)
		}
		for _, ev := range evs[:len(evs)-1] {
			if ev.Kind == EvDone {
				t.Errorf("%s: more than one EvDone", kind)
			}
		}
		b, _ := os.ReadFile(rec)
		if !strings.Contains(string(b), "---\nCTX") || !strings.HasSuffix(string(b), "hello there") {
			t.Errorf("%s: stdin not delivered: %q", kind, b)
		}
	}

	t.Setenv("GROVE_FAKE_AGENT", "broken")
	var last Event
	Run(context.Background(), Request{Kind: Claude, Bin: self, Dir: dir, Message: "x"}, func(ev Event) { last = ev })
	if last.Kind != EvDone || last.Err == nil || !strings.Contains(last.Err.Error(), "not logged in") {
		t.Errorf("broken agent = %+v", last)
	}

	if Run(context.Background(), Request{Kind: Claude, Bin: "", Dir: dir}, func(ev Event) { last = ev }); last.Err == nil && Find(Claude) == "" {
		t.Error("a missing agent should report it isn't installed")
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
