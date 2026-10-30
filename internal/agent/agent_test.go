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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/azeem-yousaf/grove/internal/usage"
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
// input line until stdin closes, reporting usage as the real one does
// (fakeTurn), with the session's totals kept in a file so that they carry
// on after --resume. A message of "wait" streams a little and then waits
// for an interrupt; "die" streams a little and exits; "quiet" reports no
// usage. Each start and message is appended to GROVE_FAKE_RECORD.
func fakeClaude() {
	rec, _ := os.OpenFile(os.Getenv("GROVE_FAKE_RECORD"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	fmt.Fprintf(rec, "start %s\n", strings.Join(os.Args[1:], " "))
	sessionFile := os.Getenv("GROVE_FAKE_RECORD") + ".session"
	var s fakeSession
	if slices.Contains(os.Args, "--resume") {
		b, _ := os.ReadFile(sessionFile)
		json.Unmarshal(b, &s)
	}
	save := func() {
		b, _ := json.Marshal(s)
		os.WriteFile(sessionFile, b, 0o644)
	}
	// A fork gets a session ID of its own.
	say := func(line string) {
		if slices.Contains(os.Args, "--fork-session") {
			line = strings.ReplaceAll(line, `"s-123"`, `"s-fork"`)
		}
		fmt.Println(line)
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 1<<20)
	n := 0 // turns this process has had
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
			if v, ok := os.LookupEnv("GROVE_TEST_ENV"); ok {
				fmt.Fprintf(rec, "env GROVE_TEST_ENV=%s\n", v) // reports the process's environment
			}
			fmt.Fprintf(rec, "user %q\n", in.Message.Content)
			n++
			msg := in.Message.Content
			if strings.HasSuffix(msg, "wait") || strings.HasSuffix(msg, "die") {
				fmt.Println(`{"type":"system","subtype":"init","session_id":"s-123"}`)
				fmt.Println(`{"type":"stream_event","event":{"type":"message_start","message":{"id":"w1","usage":{"input_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":3000,"output_tokens":1}}},"parent_tool_use_id":null}`)
				fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Thinking"}},"parent_tool_use_id":null}`)
				fmt.Println(`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":4}},"parent_tool_use_id":null}`)
				if strings.HasSuffix(msg, "die") {
					// Claude Code has already added what the turn used to
					// the session.
					s.Main = s.Main.Add(diedTokens)
					save()
					os.Exit(1)
				}
				continue
			}
			if strings.HasSuffix(msg, "quiet") {
				say(`{"type":"system","subtype":"init","session_id":"s-123"}`)
				say(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s-123"}`)
				continue
			}
			say(fakeTurn(n, &s))
			save()
		case in.Type == "control_request" && in.Request.Subtype == "interrupt":
			fmt.Fprintln(rec, "interrupt")
			s.Main = s.Main.Add(stoppedTokens)
			save()
			fmt.Println(`{"type":"control_response","response":{"subtype":"success","request_id":"grove-stop"}}`)
			fmt.Println(`{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"s-123","usage":{"input_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":3000,"output_tokens":9},"modelUsage":` + s.modelUsage() + `}`)
		}
	}
}

// fakeSession is what a fake Claude session has used so far, by the main
// conversation's model and the subagent's.
type fakeSession struct{ Main, Sub usage.Tokens }

// modelUsage is the session's modelUsage, as Claude Code writes it.
func (s fakeSession) modelUsage() string {
	model := func(t usage.Tokens) string {
		return fmt.Sprintf(`{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":%d,"costUSD":0.01}`,
			t.Input-t.CacheRead-t.CacheWrite, t.Output, t.CacheRead, t.CacheWrite)
	}
	if s.Sub.IsZero() {
		return `{"claude-haiku":` + model(s.Main) + `}`
	}
	return `{"claude-haiku":` + model(s.Main) + `,"claude-sonnet":` + model(s.Sub) + `}`
}

// The fake's per-turn figures: the main conversation of turn n (live and in
// result.usage), the subagent every turn has (only in modelUsage), and an
// interrupted turn's.
func mainTokens(n int64) usage.Tokens {
	return usage.Tokens{Input: n + 2303, Output: 25, CacheRead: 2200, CacheWrite: 100, HasCache: true}
}

var (
	subTokens     = usage.Tokens{Input: 50, Output: 7, HasCache: true}
	stoppedTokens = usage.Tokens{Input: 3007, Output: 9, CacheRead: 3000, HasCache: true}
	diedTokens    = usage.Tokens{Input: 3007, Output: 4, CacheRead: 3000, HasCache: true} // what "die" streamed
)

// turnTokens is everything turn n used, the subagent included.
func turnTokens(n int64) usage.Tokens { return mainTokens(n).Add(subTokens) }

// fakeTurn is the output of a turn that is the process's nth, adding it to
// s. It has two messages, m<n>a (with tool calls, its assistant lines
// repeating its usage per content block) and m<n>b, plus a subagent's. The
// result's usage is the main conversation's for this turn alone; its
// modelUsage adds up over the session and includes the subagent (like
// Claude Code's). The live figures agree with result.usage: input
// n+3+100+2200 = n+2303 (cache read 2200, write 100), output 25.
func fakeTurn(n int, s *fakeSession) string {
	s.Main = s.Main.Add(mainTokens(int64(n)))
	s.Sub = s.Sub.Add(subTokens)
	return fmt.Sprintf(`{"type":"system","subtype":"init","session_id":"s-123","cwd":"/w","tools":["Read","Edit"]}
{"type":"stream_event","event":{"type":"message_start","message":{"id":"m%[1]da","model":"claude-haiku","usage":{"input_tokens":%[1]d,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":1}}},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Look"}},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ing…"}},"parent_tool_use_id":null}
{"type":"assistant","message":{"id":"m%[1]da","content":[{"type":"text","text":"Looking…"},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/w/a.go"}}],"usage":{"input_tokens":%[1]d,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":1}},"parent_tool_use_id":null}
{"type":"assistant","message":{"id":"m%[1]da","content":[{"type":"tool_use","id":"t2","name":"Edit","input":{"file_path":"/w/a.go","old_string":"x","new_string":"y"}}],"usage":{"input_tokens":%[1]d,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":1}},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"message_delta","usage":{"input_tokens":%[1]d,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":20}},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"message_start","message":{"id":"sub1","usage":{"input_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}},"parent_tool_use_id":"t5"}
{"type":"assistant","message":{"id":"sub1","content":[{"type":"tool_use","id":"t9","name":"Read","input":{"file_path":"/w/sub.go"}}]},"parent_tool_use_id":"t5"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"message_start","message":{"id":"m%[1]db","usage":{"input_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":1200,"output_tokens":1}}},"parent_tool_use_id":null}
{"type":"assistant","message":{"id":"m%[1]db","content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"npm test"}}]},"parent_tool_use_id":null}
{"type":"assistant","message":{"id":"m%[1]db","content":[{"type":"text","text":"Done. Run the tests to check."}]},"parent_tool_use_id":null}
{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":5}},"parent_tool_use_id":null}
{"type":"result","subtype":"success","is_error":false,"result":"Done. Run the tests to check.","session_id":"s-123","total_cost_usd":%.4[2]f,"usage":{"input_tokens":%[3]d,"cache_creation_input_tokens":100,"cache_read_input_tokens":2200,"output_tokens":25},"modelUsage":%[4]s,"permission_denials":[{"tool_name":"Bash","tool_use_id":"t3","tool_input":{"command":"npm test"}}]}`,
		n, 0.0421*float64(n), n+3, s.modelUsage())
}

var claudeSample = fakeTurn(1, &fakeSession{})

const codexSample = `{"type":"thread.started","thread_id":"th-9"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"bash -lc ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"bash -lc ls","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"i2","type":"file_change","changes":[{"path":"src/a.ts","kind":"update"},{"path":"src/b.ts","kind":"add"}],"status":"completed"}}
{"type":"item.completed","item":{"id":"i3","item_type":"agent_message","text":"Updated a.ts and added b.ts."}}
{"type":"turn.completed","usage":{"input_tokens":12000,"cached_input_tokens":4000,"output_tokens":340,"reasoning_output_tokens":120}}`

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
	if done.Tokens != turnTokens(1) || done.TokenStatus != usage.Exact {
		t.Errorf("done tokens = %+v %v, want %+v exact", done.Tokens, done.TokenStatus, turnTokens(1))
	}
	// Live figures: each message counted once by id however often its usage
	// repeats, the subagent's left out (Claude Code streams none of its
	// messages), and an event only when the total changes.
	var live []usage.Tokens
	for _, ev := range evs {
		if ev.Kind == EvUsage {
			live = append(live, ev.Tokens)
		}
	}
	wantLive := []usage.Tokens{
		{Input: 1101, Output: 1, CacheRead: 1000, CacheWrite: 100, HasCache: true},
		{Input: 1101, Output: 20, CacheRead: 1000, CacheWrite: 100, HasCache: true},
		{Input: 2304, Output: 21, CacheRead: 2200, CacheWrite: 100, HasCache: true},
		mainTokens(1),
		turnTokens(1), // the result's figure, with the subagent
	}
	if fmt.Sprint(live) != fmt.Sprint(wantLive) {
		t.Errorf("live usage =\n%+v\nwant\n%+v", live, wantLive)
	}

	// An error result carries its message.
	p := &claudeParser{}
	p.line([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["boom"],"session_id":"s"}`))
	if ev := p.done(nil, ""); ev.Err == nil || ev.Err.Error() != "boom" {
		t.Errorf("error result = %v", ev.Err)
	}
	// No result at all: explain from stderr.
	if ev := (&claudeParser{}).done(fmt.Errorf("exit status 1"), "warn\nError: not logged in\n"); ev.Err == nil || !strings.Contains(ev.Err.Error(), "not logged in") || ev.TokenStatus != usage.Unknown {
		t.Errorf("missing result = %v %v", ev.Err, ev.TokenStatus)
	}
}

func TestClaudeUsageMalformed(t *testing.T) {
	start := func(id, u string) string {
		return `{"type":"stream_event","event":{"type":"message_start","message":{"id":"` + id + `","usage":` + u + `}},"parent_tool_use_id":null}`
	}
	for name, c := range map[string]struct {
		lines  []string
		tokens usage.Tokens
		status usage.Status
	}{
		"null usage":      {[]string{start("a", "null"), `{"type":"result","usage":null}`}, usage.Tokens{}, usage.Unknown},
		"string counts":   {[]string{start("a", `{"input_tokens":"5","output_tokens":"1"}`), `{"type":"result","usage":{"input_tokens":"5","output_tokens":2}}`}, usage.Tokens{}, usage.Unknown},
		"negative":        {[]string{`{"type":"result","usage":{"input_tokens":-5,"output_tokens":2}}`}, usage.Tokens{}, usage.Unknown},
		"fractional":      {[]string{`{"type":"result","usage":{"input_tokens":1.5,"output_tokens":2}}`}, usage.Tokens{}, usage.Unknown},
		"no output side":  {[]string{`{"type":"result","usage":{"input_tokens":5}}`}, usage.Tokens{}, usage.Unknown},
		"not an object":   {[]string{`{"type":"result","usage":[1,2]}`}, usage.Tokens{}, usage.Unknown},
		"lone delta":      {[]string{`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":3}},"parent_tool_use_id":null}`}, usage.Tokens{}, usage.Unknown},
		"no cache fields": {[]string{`{"type":"result","modelUsage":{"m":{"inputTokens":5,"outputTokens":2}}}`}, usage.Tokens{Input: 5, Output: 2}, usage.Exact},
		"two models":      {[]string{`{"type":"result","modelUsage":{"a":{"inputTokens":5,"outputTokens":2,"cacheReadInputTokens":10},"b":{"inputTokens":1,"outputTokens":1}}}`}, usage.Tokens{Input: 16, Output: 3, CacheRead: 10, HasCache: true}, usage.Exact},
		// Without modelUsage, result.usage may leave out subagents.
		"usage only": {[]string{`{"type":"result","usage":{"input_tokens":5,"output_tokens":2}}`}, usage.Tokens{Input: 5, Output: 2}, usage.Partial},
		"bad models": {[]string{`{"type":"result","usage":{"input_tokens":5,"output_tokens":2},"modelUsage":{"a":{"inputTokens":"5","outputTokens":2}}}`}, usage.Tokens{Input: 5, Output: 2}, usage.Partial},
		"no models":  {[]string{`{"type":"result","usage":{"input_tokens":5,"output_tokens":2},"modelUsage":{}}`}, usage.Tokens{Input: 5, Output: 2}, usage.Partial},
		// A broken delta leaves the message's last good figures.
		"bad delta": {[]string{start("a", `{"input_tokens":5,"cache_read_input_tokens":10,"output_tokens":1}`), `{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":"x"}},"parent_tool_use_id":null}`},
			usage.Tokens{Input: 15, Output: 1, CacheRead: 10, HasCache: true}, usage.Partial},
		// The same message started twice still counts once.
		"repeated id": {[]string{start("a", `{"input_tokens":5,"output_tokens":1}`), start("a", `{"input_tokens":5,"output_tokens":1}`)},
			usage.Tokens{Input: 5, Output: 1}, usage.Partial},
		// A result with no usable usage falls back to the live figures.
		"result without usage": {[]string{start("a", `{"input_tokens":5,"output_tokens":1}`), `{"type":"result","subtype":"success"}`},
			usage.Tokens{Input: 5, Output: 1}, usage.Partial},
	} {
		p := &claudeParser{}
		for _, l := range c.lines {
			for _, ev := range p.line([]byte(l)) {
				if ev.Kind == EvUsage && ev.Tokens.IsZero() {
					t.Errorf("%s: zero usage reported", name)
				}
			}
		}
		if ev := p.done(nil, ""); ev.Tokens != c.tokens || ev.TokenStatus != c.status {
			t.Errorf("%s: done = %+v %v, want %+v %v", name, ev.Tokens, ev.TokenStatus, c.tokens, c.status)
		}
	}
}

func TestClaudeUsageBase(t *testing.T) {
	result := `{"type":"result","usage":{"input_tokens":5,"output_tokens":2},"modelUsage":{"m":{"inputTokens":105,"outputTokens":12}}}`
	run := func(p *claudeParser) Event {
		p.line([]byte(result))
		return p.done(nil, "")
	}
	// The turn is the session's total less what it had used before.
	ev := run(&claudeParser{base: usage.Tokens{Input: 95, Output: 9}})
	if ev.Tokens != (usage.Tokens{Input: 10, Output: 3}) || ev.TokenStatus != usage.Exact || ev.SessionTotal != (usage.Tokens{Input: 105, Output: 12}) {
		t.Errorf("with a base = %+v", ev)
	}
	// Without one, or with one that doesn't fit (the session was used
	// elsewhere), only the main conversation's figure is known.
	for name, p := range map[string]*claudeParser{
		"unknown": {noBase: true},
		"above":   {base: usage.Tokens{Input: 200, Output: 9}},
		"too big": {base: usage.Tokens{Input: 102, Output: 9}}, // leaves less than result.usage
	} {
		if ev := run(p); ev.Tokens != (usage.Tokens{Input: 5, Output: 2}) || ev.TokenStatus != usage.Partial || ev.SessionTotal != (usage.Tokens{Input: 105, Output: 12}) {
			t.Errorf("%s base = %+v", name, ev)
		}
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
	// input_tokens already includes the cached ones, output_tokens the
	// reasoning ones.
	want0 := usage.Tokens{Input: 12000, Output: 340, CacheRead: 4000, HasCache: true}
	if done.Err != nil || done.Cost != "" || done.Tokens != want0 || done.TokenStatus != usage.Exact {
		t.Errorf("done = %+v", done)
	}

	p := &codexParser{}
	p.line([]byte(`{"type":"turn.failed","error":{"message":"rate limited"}}`))
	if ev := p.done(nil, ""); ev.Err == nil || ev.Err.Error() != "rate limited" || ev.TokenStatus != usage.Unknown {
		t.Errorf("turn.failed = %v %v", ev.Err, ev.TokenStatus)
	}
}

func TestCodexUsage(t *testing.T) {
	for name, c := range map[string]struct {
		lines  []string
		exit   error
		tokens usage.Tokens
		status usage.Status
		live   int // EvUsage events
	}{
		"several turns add up": {[]string{
			`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":60,"output_tokens":10}}`,
			`{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":150,"output_tokens":20,"reasoning_output_tokens":5}}`,
		}, nil, usage.Tokens{Input: 300, Output: 30, CacheRead: 210, HasCache: true}, usage.Exact, 2},
		"no cache field": {[]string{`{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10}}`},
			nil, usage.Tokens{Input: 100, Output: 10}, usage.Exact, 1},
		"cache writes": {[]string{`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"cache_write_input_tokens":10,"output_tokens":5}}`},
			nil, usage.Tokens{Input: 100, Output: 5, CacheRead: 40, CacheWrite: 10, HasCache: true}, usage.Exact, 1},
		"failed with usage": {[]string{`{"type":"turn.failed","error":{"message":"boom"},"usage":{"input_tokens":50,"cached_input_tokens":0,"output_tokens":2}}`},
			nil, usage.Tokens{Input: 50, Output: 2, HasCache: true}, usage.Exact, 1},
		"completed then error": {[]string{
			`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":10}}`,
			`{"type":"error","message":"stream closed"}`,
		}, nil, usage.Tokens{Input: 100, Output: 10, HasCache: true}, usage.Exact, 1},
		"stopped before a turn completed": {[]string{`{"type":"turn.started"}`}, errStopped, usage.Tokens{}, usage.Unknown, 0},
		"null usage":                      {[]string{`{"type":"turn.completed","usage":null}`}, nil, usage.Tokens{}, usage.Unknown, 0},
		"no usage":                        {[]string{`{"type":"turn.completed"}`}, nil, usage.Tokens{}, usage.Unknown, 0},
		"strings":                         {[]string{`{"type":"turn.completed","usage":{"input_tokens":"100","output_tokens":10}}`}, nil, usage.Tokens{}, usage.Unknown, 0},
		"negative":                        {[]string{`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":-1,"output_tokens":10}}`}, nil, usage.Tokens{}, usage.Unknown, 0},
		"more cached than input":          {[]string{`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":60,"output_tokens":1}}`}, nil, usage.Tokens{}, usage.Unknown, 0},
		"missing output":                  {[]string{`{"type":"turn.completed","usage":{"input_tokens":10}}`}, nil, usage.Tokens{}, usage.Unknown, 0},
	} {
		p := &codexParser{}
		live := 0
		for _, l := range c.lines {
			for _, ev := range p.line([]byte(l)) {
				if ev.Kind == EvUsage {
					live++
				}
			}
		}
		if ev := p.done(c.exit, ""); ev.Tokens != c.tokens || ev.TokenStatus != c.status || live != c.live {
			t.Errorf("%s: done = %+v %v after %d EvUsage, want %+v %v after %d", name, ev.Tokens, ev.TokenStatus, live, c.tokens, c.status, c.live)
		}
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

	args, in := codexArgs(Request{Message: "and now?", Context: "CTX", History: []Turn{{User: "hi", Agent: "hello"}}}, DefaultReplay)
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
	if args, _ := codexArgs(req, DefaultReplay); strings.Join(args, " ") != "exec --json --sandbox read-only -" {
		t.Errorf("read-only codex args = %q", args)
	}
	if strings.Contains(joined, "--allowedTools Bash ") || strings.Contains(joined, " PowerShell ") {
		t.Errorf("a read-only agent asks before running commands: %s", joined)
	}

	// The planner running a plan may run any command, but still not edit.
	req.Commands = true
	args = claudeArgs(req)
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, " Bash PowerShell ") || !strings.Contains(joined, "--disallowedTools Edit") || args[len(args)-1] != "Bash(ls)" {
		t.Errorf("claude args with commands: %s", joined)
	}
	if args, _ := codexArgs(req, DefaultReplay); strings.Join(args, " ") != "exec --json --sandbox workspace-write -" {
		t.Errorf("codex args with commands = %q", args)
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
	cargs, _ := codexArgs(Request{Tools: tools}, DefaultReplay)
	if j := strings.Join(cargs, " "); !strings.Contains(j, `-c mcp_servers.grove.command="C:\\bin\\grove.exe" -c mcp_servers.grove.args=["mcp-group","--as","feature/x"]`) {
		t.Errorf("codex args with tools: %s", j)
	}
	for in, want := range map[string]string{
		"/src/shop-api":       "Read(//src/shop-api/**)",
		"/src/shop-api/":      "Read(//src/shop-api/**)",
		`C:\src\shop-api`:     "Read(//c/src/shop-api/**)",
		`D:\Work\My Repo\ui\`: "Read(//d/Work/My Repo/ui/**)",
	} {
		if got := ReadRule(in); got != want {
			t.Errorf("ReadRule(%q) = %q, want %q", in, got, want)
		}
	}
	args = claudeArgs(Request{ReadDirs: []string{"/src/shop-ui"}, Allow: []string{"Bash(ls)"}})
	if j := strings.Join(args, " "); !strings.Contains(j, "Read(//src/shop-ui/**) Bash(ls)") || strings.Contains(j, "--add-dir") {
		t.Errorf("claude args with read dirs: %s", j)
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
	if done.Tokens != turnTokens(1) || done.TokenStatus != usage.Exact {
		t.Errorf("first turn tokens = %+v %v", done.Tokens, done.TokenStatus)
	}
	// The next message goes to the same process, and its usage is its own,
	// not added to the first's.
	req.Message, req.Context, req.Session = "and again", "", done.Session
	evs = turn(t, r, ctx, req)
	if done := evs[len(evs)-1]; done.Err != nil || done.Tokens != turnTokens(2) || done.TokenStatus != usage.Exact {
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
	evs = turn(t, r, ctx, req)
	done = evs[len(evs)-1]
	if done.Tokens != turnTokens(1) || done.TokenStatus != usage.Exact {
		t.Errorf("turn after --resume = %+v %v", done.Tokens, done.TokenStatus)
	}
	rec = record(t)
	last := rec[strings.LastIndex(rec, "start "):]
	last = last[:strings.IndexByte(last, '\n')]
	if strings.Count(rec, "start ") != 2 || !strings.Contains(last, "--resume s-123") || !strings.HasSuffix(last, "Bash(npm test)") {
		t.Errorf("approving should restart with --resume and the rule:\n%s", rec)
	}

	// After grove restarts, a new Runner knows the session's total only if
	// it is passed in; otherwise just the main conversation's figure.
	r.Stop(dir)
	r2 := NewRunner()
	defer r2.Stop(dir)
	req.Message = "after a restart"
	evs = turn(t, r2, ctx, req)
	if done := evs[len(evs)-1]; done.Tokens != mainTokens(1) || done.TokenStatus != usage.Partial {
		t.Errorf("resumed turn without a total = %+v %v", done.Tokens, done.TokenStatus)
	}
	r2.Stop(dir)
	r3 := NewRunner()
	defer r3.Stop(dir)
	req.Message, req.SessionTotal = "with the total", evs[len(evs)-1].SessionTotal
	evs = turn(t, r3, ctx, req)
	if done := evs[len(evs)-1]; done.Tokens != turnTokens(1) || done.TokenStatus != usage.Exact {
		t.Errorf("resumed turn with its total = %+v %v", done.Tokens, done.TokenStatus)
	}
}

func TestClaudeForkLeavesKeptProcess(t *testing.T) {
	bin, dir := fakeAgent(t, "claude")
	r := NewRunner()
	defer r.Stop(dir)
	ctx := context.Background()
	req := Request{Kind: Claude, Bin: bin, Dir: dir, Message: "hello", ReadOnly: true, Slot: "lead"}
	evs := turn(t, r, ctx, req)
	req.Session = evs[len(evs)-1].Session

	// The main session is busy with a long turn.
	mainCtx, stopMain := context.WithCancel(ctx)
	busy, mainDone := make(chan struct{}), make(chan []Event)
	go func() {
		var evs []Event
		once := false
		main := req
		main.Message = "wait"
		r.Run(mainCtx, main, func(ev Event) {
			evs = append(evs, ev)
			if ev.Kind == EvDelta && !once {
				once = true
				close(busy)
			}
		})
		mainDone <- evs
	}()
	<-busy

	fork := req
	fork.Message, fork.Fork = "quick", true
	evs = turn(t, r, ctx, fork)
	if done := evs[len(evs)-1]; done.Err != nil || done.Session != "s-fork" {
		t.Fatalf("fork turn = %+v", done)
	}
	rec := record(t)
	if !strings.Contains(rec, "--resume s-123 --fork-session") {
		t.Errorf("the fork should resume the session with --fork-session:\n%s", rec)
	}
	r.mu.Lock()
	n := len(r.procs)
	r.mu.Unlock()
	if n != 1 {
		t.Errorf("the fork's process should end with its turn; %d kept", n)
	}

	// The main turn was never disturbed, and its process is kept.
	stopMain()
	evs = <-mainDone
	if done := evs[len(evs)-1]; done.Session != "s-123" {
		t.Errorf("main turn = %+v", done)
	}
	req.Message = "again"
	evs = turn(t, r, ctx, req)
	if done := evs[len(evs)-1]; done.Err != nil || done.Session != "s-123" {
		t.Fatalf("main turn after the fork = %+v", done)
	}
	if rec := record(t); strings.Count(rec, "start ") != 2 {
		t.Errorf("want the kept process and one fork:\n%s", rec)
	}
}

func TestHookArgs(t *testing.T) {
	hook := []string{`C:\Program Files\grove\grove.exe`, "agent-hook", "--dir", "/s/grove/control", "--as", "/w/it's"}
	args := claudeArgs(Request{Hook: hook, ReadOnly: true, Commands: true, Allow: []string{"Bash(ls)"}})
	i := slices.Index(args, "--settings")
	if i < 0 {
		t.Fatalf("no --settings: %q", args)
	}
	var s struct {
		Hooks struct {
			PreToolUse []struct {
				Matcher string
				Hooks   []struct {
					Type, Command string
					Timeout       int
				}
			}
		}
	}
	if err := json.Unmarshal([]byte(args[i+1]), &s); err != nil {
		t.Fatalf("--settings isn't JSON: %v %s", err, args[i+1])
	}
	pre := s.Hooks.PreToolUse
	if len(pre) != 1 || pre[0].Matcher != "Bash|PowerShell" || len(pre[0].Hooks) != 1 {
		t.Fatalf("hook settings = %+v", s)
	}
	h := pre[0].Hooks[0]
	want := `'C:\Program Files\grove\grove.exe' agent-hook --dir /s/grove/control --as '/w/it'\''s'`
	if h.Type != "command" || h.Command != want || h.Timeout != 30 {
		t.Errorf("hook = %+v, want command %s", h, want)
	}
	if j := strings.Join(args, " "); args[len(args)-1] != "Bash(ls)" || !strings.Contains(j, "--disallowedTools Edit") || !strings.Contains(j, " Bash PowerShell ") {
		t.Errorf("the planner's modes should still apply with a hook: %s", j)
	}
	if slices.Contains(claudeArgs(Request{}), "--settings") {
		t.Error("no hook should mean no --settings")
	}
}

func TestEnvReachesAgents(t *testing.T) {
	bin, dir := fakeAgent(t, "claude")
	r := NewRunner()
	defer r.Stop(dir)
	ctx := context.Background()
	req := Request{Kind: Claude, Bin: bin, Dir: dir, Message: "hi", Env: []string{"GROVE_TEST_ENV=one"}}
	done := turn(t, r, ctx, req)
	req.Session = done[len(done)-1].Session
	turn(t, r, ctx, req)
	if rec := record(t); strings.Count(rec, "start ") != 1 || !strings.Contains(rec, "env GROVE_TEST_ENV=one") {
		t.Fatalf("Env should reach the kept process:\n%s", rec)
	}
	// Other values need a new process, which resumes the conversation.
	req.Env = []string{"GROVE_TEST_ENV=two"}
	turn(t, r, ctx, req)
	rec := record(t)
	if strings.Count(rec, "start ") != 2 || !strings.Contains(rec, "env GROVE_TEST_ENV=two") || !strings.Contains(rec[strings.LastIndex(rec, "start "):], "--resume s-123") {
		t.Errorf("an Env change should restart with --resume:\n%s", rec)
	}
	// So does a new hook.
	req.Hook = []string{"grove", "agent-hook"}
	turn(t, r, ctx, req)
	if rec := record(t); strings.Count(rec, "start ") != 3 || !strings.Contains(rec, "--settings") {
		t.Errorf("a Hook change should restart:\n%s", rec)
	}

	bin, dir = fakeAgent(t, "codex")
	turn(t, r, ctx, Request{Kind: Codex, Bin: bin, Dir: dir, Message: "hi", Env: []string{"GROVE_FAKE_RECORD=" + filepath.Join(dir, "env.txt")}})
	if _, err := os.Stat(filepath.Join(dir, "env.txt")); err != nil {
		t.Errorf("Env should reach codex: %v", err)
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
	// An interrupted turn still ends with a result and its usage.
	if want := (usage.Tokens{Input: 3007, Output: 9, CacheRead: 3000, HasCache: true}); done.Tokens != want || done.TokenStatus != usage.Exact {
		t.Errorf("stopped turn tokens = %+v %v, want %+v exact", done.Tokens, done.TokenStatus, want)
	}
	if !strings.Contains(record(t), "interrupt") {
		t.Error("stopping should interrupt the turn, not kill the process")
	}
	evs = turn(t, r, context.Background(), Request{Kind: Claude, Bin: bin, Dir: dir, Message: "next", Session: "s-123"})
	if evs[len(evs)-1].Err != nil || strings.Count(record(t), "start ") != 1 {
		t.Errorf("the process should still be usable after a stop:\n%s", record(t))
	}
}

func TestClaudeUsageCutShort(t *testing.T) {
	bin, dir := fakeAgent(t, "claude")
	r := NewRunner()
	defer r.Stop(dir)
	// The process dies mid-turn: only the live figures, a lower bound.
	evs := turn(t, r, context.Background(), Request{Kind: Claude, Bin: bin, Dir: dir, Message: "die"})
	done := evs[len(evs)-1]
	if done.Err == nil || done.Tokens != diedTokens || done.TokenStatus != usage.Partial {
		t.Errorf("killed turn = %+v, want %+v partial", done, diedTokens)
	}
	// A result without usage, and nothing live: unknown, not zero.
	evs = turn(t, r, context.Background(), Request{Kind: Claude, Bin: bin, Dir: dir, Message: "quiet"})
	done = evs[len(evs)-1]
	if done.Err != nil || !done.Tokens.IsZero() || done.TokenStatus != usage.Unknown {
		t.Errorf("turn without usage = %+v", done)
	}
	for _, ev := range evs {
		if ev.Kind == EvUsage {
			t.Errorf("no usage should mean no EvUsage: %+v", ev)
		}
	}
}

// A turn killed mid-way still adds to the session in Claude Code, so the
// next turn mustn't subtract the total from before it: that would count the
// killed turn again.
func TestClaudeKilledTurnNotCountedTwice(t *testing.T) {
	bin, dir := fakeAgent(t, "claude")
	r := NewRunner()
	defer r.Stop(dir)
	ctx := context.Background()
	evs := turn(t, r, ctx, Request{Kind: Claude, Bin: bin, Dir: dir, Message: "hello"})
	first := evs[len(evs)-1]
	if first.TokenStatus != usage.Exact || first.SessionTotal.IsZero() {
		t.Fatalf("first turn = %+v", first)
	}
	req := Request{Kind: Claude, Bin: bin, Dir: dir, Session: "s-123", SessionTotal: first.SessionTotal, Message: "die"}
	evs = turn(t, r, ctx, req)
	if done := evs[len(evs)-1]; done.TokenStatus != usage.Partial || !done.SessionTotal.IsZero() {
		t.Fatalf("killed turn = %+v", done)
	}
	// The resumed turn, even when the caller still passes the old total, is
	// only the main conversation's figure: not the killed turn's as well.
	req.Message = "go on"
	evs = turn(t, r, ctx, req)
	done := evs[len(evs)-1]
	if done.Tokens != mainTokens(1) || done.TokenStatus != usage.Partial {
		t.Errorf("turn after a killed one = %+v %v, want %+v partial", done.Tokens, done.TokenStatus, mainTokens(1))
	}
	// Its total is known again, so the turn after it is exact.
	req.Message, req.SessionTotal = "and on", done.SessionTotal
	evs = turn(t, r, ctx, req)
	if done := evs[len(evs)-1]; done.Tokens != turnTokens(2) || done.TokenStatus != usage.Exact {
		t.Errorf("next turn = %+v %v", done.Tokens, done.TokenStatus)
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

// codexStdin runs one Codex turn through the fake binary and returns the
// conversation part of what it read on stdin.
func codexStdin(t *testing.T, r *Runner, req Request) (history, all string) {
	t.Helper()
	bin, dir := fakeAgent(t, "codex")
	req.Kind, req.Bin, req.Dir = Codex, bin, dir
	if done := turn(t, r, context.Background(), req); done[len(done)-1].Err != nil {
		t.Fatalf("codex done = %+v", done[len(done)-1])
	}
	_, all, _ = strings.Cut(record(t), "\n---\n")
	_, history, _ = strings.Cut(all, "## Conversation so far\n\n")
	history, _, _ = strings.Cut(history, "## Request\n\n")
	return history, all
}

func TestCodexReplay(t *testing.T) {
	// A short history goes as it is.
	short := []Turn{{User: "hi", Agent: "hello"}, {User: "fix it", Agent: "done"}}
	h, all := codexStdin(t, NewRunner(), Request{Message: "and now?", Context: "CTX", History: short})
	if h != "User: hi\n\nYou: hello\n\nUser: fix it\n\nYou: done\n\n" || !strings.HasPrefix(all, "CTX\n\n") || !strings.HasSuffix(all, "## Request\n\nand now?") {
		t.Errorf("short history = %q", all)
	}

	// A long one: the last Full whole, older replies cut, within the budget.
	var long []Turn
	for i := range 40 {
		long = append(long, Turn{User: fmt.Sprintf("ask %d", i), Agent: fmt.Sprintf("reply %d ", i) + strings.Repeat("x", 1000)})
	}
	r := NewRunner()
	r.Replay = Replay{Full: 3, Brief: 50, Budget: 5000}
	ctx := strings.Repeat("C", 20000) // never cut, whatever the budget
	msg := "the current message " + strings.Repeat("m", 10000)
	h, all = codexStdin(t, r, Request{Message: msg, Context: ctx, History: long})
	if n := utf8.RuneCountInString(h); n > 5000+100 {
		t.Errorf("history is %d characters, over the budget", n)
	}
	if !strings.HasPrefix(all, ctx+"\n\n") || !strings.HasSuffix(all, "## Request\n\n"+msg) {
		t.Error("the briefing and the message must be sent whole")
	}
	for i := 37; i < 40; i++ {
		if !strings.Contains(h, "User: ask "+strconv.Itoa(i)+"\n\nYou: "+long[i].Agent+"\n\n") {
			t.Errorf("turn %d should be whole", i)
		}
	}
	if !strings.Contains(h, "User: ask 36\n\nYou: reply 36 "+strings.Repeat("x", 41)+"…\n\n") {
		t.Errorf("older replies should be cut: %q", h)
	}
	m := regexp.MustCompile(`^\((\d+) earlier turns left out\.\)\n\n`).FindStringSubmatch(h)
	if m == nil {
		t.Fatalf("no left-out line: %q", h[:min(len(h), 200)])
	}
	left, _ := strconv.Atoi(m[1])
	if first := fmt.Sprintf("User: ask %d\n\n", left); !strings.Contains(h, first) || strings.Contains(h, fmt.Sprintf("User: ask %d\n\n", left-1)) {
		t.Errorf("%d left out, but the oldest sent isn't turn %d: %q", left, left, h)
	}
	if strings.Count(h, "User: ask ") != 40-left {
		t.Errorf("sent %d turns, want %d", strings.Count(h, "User: ask "), 40-left)
	}

	// One huge reply among the newest is cut rather than crowding the rest out.
	big := []Turn{{User: "a", Agent: "small"}, {User: "b", Agent: strings.Repeat("y", 10000)}}
	h, _ = codexStdin(t, r, Request{Message: "next", History: big})
	if h != "User: a\n\nYou: small\n\nUser: b\n\nYou: "+strings.Repeat("y", 50)+"…\n\n" {
		t.Errorf("huge reply = %q", h)
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

func TestCommandOutput(t *testing.T) {
	p := &claudeParser{}
	evs := feed(p, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}},{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"/w/a.go"}}]},"parent_tool_use_id":null}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t2","content":"package a"}]},"parent_tool_use_id":null}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[{"type":"text","text":"--- FAIL: TestA\nFAIL\n"}]}]},"parent_tool_use_id":null}
`)
	var out []Event
	for _, ev := range evs {
		if ev.Kind == EvOutput {
			out = append(out, ev)
		}
	}
	if len(out) != 1 || out[0].Tool != "Bash" || out[0].Detail != "go test ./..." || !out[0].Failed || out[0].Text != "--- FAIL: TestA\nFAIL" {
		t.Errorf("claude output events = %+v (only shell commands, once each)", out)
	}

	evs = feed(&codexParser{}, `{"type":"item.completed","item":{"type":"command_execution","command":"npm test","aggregated_output":"ok 3 tests\n","exit_code":0,"status":"completed"}}
`)
	if len(evs) != 2 || evs[0].Kind != EvOutput || evs[0].Failed || evs[0].Text != "ok 3 tests" || evs[0].Detail != "npm test" {
		t.Errorf("codex output events = %+v", evs)
	}

	long := strings.Repeat("line of output\n", 1000)
	if got := tail(long); len(got) > outputTail || !strings.HasPrefix(got, "line of output") {
		t.Errorf("tail kept %d bytes starting %q", len(got), got[:20])
	}
}
