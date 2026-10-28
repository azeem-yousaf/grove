package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// readOnlyGit are git commands an agent may run without asking; they can't
// change the repository.
var readOnlyGit = []string{"status", "diff", "log", "show", "blame"}

// shells are Claude Code's command tools; on Windows it uses PowerShell.
var shells = []string{"Bash", "PowerShell"}

// claudeArgs starts `claude -p` taking one JSON message per turn on stdin.
// File edits inside the working folder are accepted automatically
// (acceptEdits); anything else that needs permission, such as a shell
// command or a file outside the worktree, is denied and reported in the
// turn's result so grove can ask the user.
//
// A read-only agent (the planner) runs in the default permission mode with
// the editing tools removed, so it can only read; it may also use the
// read-only tools of Claude's Atlassian connector to look at Jira.
func claudeArgs(req Request) []string {
	mode := "acceptEdits"
	if req.ReadOnly {
		mode = "default"
	}
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-mode", mode,
	}
	if req.Session != "" {
		args = append(args, "--resume", req.Session)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if cfg := mcpConfig(req.Tools); cfg != "" {
		args = append(args, "--mcp-config", cfg)
	}
	if req.ReadOnly {
		args = append(args, "--disallowedTools")
		args = append(args, editTools...)
	}
	// --allowedTools takes several values, so it goes last.
	args = append(args, "--allowedTools")
	for _, shell := range shells {
		for _, c := range readOnlyGit {
			args = append(args, ClaudeRule(shell, "git "+c), ClaudeRule(shell, "git "+c+" *"))
		}
	}
	if req.ReadOnly {
		for _, t := range jiraReadTools {
			args = append(args, atlassianTools+t)
		}
	}
	for _, s := range req.Tools {
		for _, t := range s.Allow {
			args = append(args, "mcp__"+s.Name+"__"+t)
		}
	}
	for _, d := range req.ReadDirs {
		args = append(args, ReadRule(d))
	}
	return append(args, req.Allow...)
}

// ReadRule lets Claude read everything under dir, and nothing more: edits
// there still need permission, which a turn never gets. Claude Code matches
// absolute paths written as //path, with Windows paths in POSIX form
// (C:\src\app is //c/src/app).
func ReadRule(dir string) string {
	p := strings.TrimRight(strings.ReplaceAll(dir, "\\", "/"), "/")
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + strings.ToLower(p[:1]) + p[2:]
	}
	return "Read(/" + p + "/**)"
}

// mcpConfig is the --mcp-config JSON for tool servers, or "" for none. It
// adds to the user's own MCP servers rather than replacing them.
func mcpConfig(tools []ToolServer) string {
	if len(tools) == 0 {
		return ""
	}
	servers := map[string]any{}
	for _, s := range tools {
		servers[s.Name] = map[string]any{"type": "stdio", "command": s.Command, "args": s.Args}
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": servers})
	return string(b)
}

// editTools change files; a read-only agent doesn't get them.
var editTools = []string{"Edit", "MultiEdit", "Write", "NotebookEdit"}

// atlassianTools prefixes the tools of the Atlassian connector from
// claude.ai, as Claude Code names them.
const atlassianTools = "mcp__claude_ai_Atlassian__"

// jiraReadTools are the connector's tools that only read, which the planner
// may use without asking. Creating or editing issues stays refused.
var jiraReadTools = []string{
	"getAccessibleAtlassianResources", "atlassianUserInfo", "getVisibleJiraProjects",
	"getJiraIssue", "searchJiraIssuesUsingJql", "getJiraIssueRemoteIssueLinks",
	"getConfluencePage", "searchConfluenceUsingCql", "search", "fetch",
}

// claudeInput is the stdin line that starts a turn.
func claudeInput(req Request) []byte {
	var in strings.Builder
	if req.Context != "" {
		in.WriteString(req.Context)
		in.WriteString("\n\n")
	}
	in.WriteString("## Request\n\n")
	in.WriteString(req.Message)
	b, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": in.String()},
	})
	return append(b, '\n')
}

// claudeInterrupt ends the turn in progress; the process stays usable.
var claudeInterrupt = []byte(`{"type":"control_request","request_id":"grove-stop","request":{"subtype":"interrupt"}}` + "\n")

// endGrace is how long a Claude process gets to wind down, after being
// interrupted or having its input closed, before it is killed.
const endGrace = 5 * time.Second

// claudeProc is a Claude Code process kept between turns.
type claudeProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stderr  tailBuffer
	exited  chan struct{} // closed once the process has exited and its output is read
	waitErr error         // set before exited closes
	endOnce sync.Once

	// What it was started with; a turn needing anything else gets a new
	// process.
	bin, model string
	allow      []string
	tools      string   // mcpConfig it was started with
	readDirs   []string // ReadDirs it was started with

	curMu sync.Mutex
	cur   *claudeTurn // the turn in progress, if any

	// Guarded by the Runner's lock; session is also written by the turn
	// while busy.
	session string // conversation it is in, "" until its first turn starts one
	busy    bool
	idle    *time.Timer
}

// claudeTurn receives the output of one turn.
type claudeTurn struct {
	lines chan []byte
	done  chan struct{} // closed when the turn stops reading
}

func startClaude(bin string, req Request) (*claudeProc, error) {
	p := &claudeProc{
		exited:   make(chan struct{}),
		bin:      bin,
		model:    req.Model,
		allow:    slices.Clone(req.Allow),
		tools:    mcpConfig(req.Tools),
		readDirs: slices.Clone(req.ReadDirs),
		session:  req.Session,
	}
	p.cmd = exec.Command(bin, claudeArgs(req)...)
	p.cmd.Dir = req.Dir
	p.cmd.Stderr = &p.stderr
	var err error
	if p.stdin, err = p.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := p.cmd.Start(); err != nil {
		return nil, err
	}
	go p.read(out)
	return p, nil
}

// read hands each output line to the turn in progress. Lines between turns
// belong to none and are dropped.
func (p *claudeProc) read(out io.Reader) {
	r := bufio.NewReaderSize(out, 64*1024)
	for {
		b, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(b)) > 0 {
			p.curMu.Lock()
			t := p.cur
			p.curMu.Unlock()
			if t != nil {
				select {
				case t.lines <- b:
				case <-t.done:
				}
			}
		}
		if err != nil {
			break
		}
	}
	p.waitErr = p.cmd.Wait()
	close(p.exited)
}

func (p *claudeProc) fits(bin string, req Request) bool {
	return !p.ended() && p.bin == bin && p.model == req.Model && p.session == req.Session && slices.Equal(p.allow, req.Allow) && p.tools == mcpConfig(req.Tools) && slices.Equal(p.readDirs, req.ReadDirs)
}

func (p *claudeProc) ended() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// end closes the process's input, which makes it exit once it has nothing
// left to do, and kills it if it hasn't after endGrace.
func (p *claudeProc) end() {
	p.endOnce.Do(func() {
		p.stdin.Close()
		go func() {
			select {
			case <-p.exited:
			case <-time.After(endGrace):
				p.cmd.Process.Kill()
			}
		}()
	})
}

// turn sends one message and reports the turn's events until its result,
// returning the final EvDone for the caller to emit. Cancelling ctx
// interrupts the turn, which leaves the process ready for the next one.
func (p *claudeProc) turn(ctx context.Context, req Request, emit func(Event)) Event {
	t := &claudeTurn{lines: make(chan []byte), done: make(chan struct{})}
	p.curMu.Lock()
	p.cur = t
	p.curMu.Unlock()
	defer func() {
		p.curMu.Lock()
		p.cur = nil
		p.curMu.Unlock()
		close(t.done)
	}()

	parse := &claudeParser{session: p.session}
	finish := func(ev Event, stopped bool) Event {
		if stopped {
			ev.Err = errStopped
		}
		p.session = parse.session
		return ev
	}
	if _, err := p.stdin.Write(claudeInput(req)); err != nil {
		// The process ended before the turn could start; its exit explains why.
		select {
		case <-p.exited:
			return finish(parse.done(p.waitErr, p.stderr.String()), false)
		case <-time.After(endGrace):
			return finish(parse.done(err, p.stderr.String()), false)
		}
	}
	cancel, stopped := ctx.Done(), false
	var kill <-chan time.Time // after an interrupt, in case it isn't heeded
	for {
		select {
		case b := <-t.lines:
			for _, ev := range parse.line(b) {
				emit(ev)
			}
			if parse.result != nil {
				return finish(parse.done(nil, ""), stopped)
			}
		case <-p.exited:
			return finish(parse.done(p.waitErr, p.stderr.String()), stopped)
		case <-cancel:
			cancel, stopped = nil, true
			p.stdin.Write(claudeInterrupt)
			kill = time.After(endGrace)
		case <-kill:
			p.cmd.Process.Kill()
		}
	}
}

// ClaudeRule is the permission rule that lets Claude run exactly command
// with shell, the tool it asked for: "Bash", or "PowerShell" on Windows.
func ClaudeRule(shell, command string) string { return shell + "(" + command + ")" }

type claudeParser struct {
	session string
	result  *claudeResult
	sawText bool
}

type claudeLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Session string `json:"session_id"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	claudeResult
}

type claudeResult struct {
	IsError bool     `json:"is_error"`
	Result  string   `json:"result"`
	Errors  []string `json:"errors"`
	Cost    float64  `json:"total_cost_usd"`
	Denials []struct {
		Tool  string          `json:"tool_name"`
		Input json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
}

func (p *claudeParser) line(b []byte) []Event {
	var l claudeLine
	if json.Unmarshal(b, &l) != nil {
		return nil
	}
	sub := l.ParentToolUseID != nil && *l.ParentToolUseID != ""
	switch l.Type {
	case "system":
		if l.Subtype == "init" && l.Session != "" {
			p.session = l.Session
			return []Event{{Kind: EvSession, Session: l.Session}}
		}
	case "stream_event":
		if !sub && l.Event.Type == "content_block_delta" && l.Event.Delta.Type == "text_delta" {
			return []Event{{Kind: EvDelta, Text: l.Event.Delta.Text}}
		}
	case "assistant":
		if sub {
			return nil // a subagent's own steps
		}
		var evs []Event
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					p.sawText = true
					evs = append(evs, Event{Kind: EvText, Text: c.Text})
				}
			case "tool_use":
				detail, path := toolDetail(c.Name, c.Input)
				evs = append(evs, Event{Kind: EvTool, Tool: c.Name, Detail: detail, Path: path})
			}
		}
		return evs
	case "result":
		if l.Session != "" {
			p.session = l.Session
		}
		r := l.claudeResult
		p.result = &r
		// The final answer is normally already shown as an assistant
		// message; only use the result text when nothing was.
		if !p.sawText && !r.IsError && strings.TrimSpace(r.Result) != "" {
			return []Event{{Kind: EvText, Text: r.Result}}
		}
	}
	return nil
}

func (p *claudeParser) done(exitErr error, stderr string) Event {
	ev := Event{Kind: EvDone, Session: p.session}
	r := p.result
	if r == nil {
		ev.Err = processErr("Claude Code", exitErr, stderr)
		return ev
	}
	if r.Cost > 0 {
		ev.Cost = fmt.Sprintf("$%.2f", r.Cost)
	}
	for _, d := range r.Denials {
		detail, path := toolDetail(d.Tool, d.Input)
		ev.Denied = append(ev.Denied, Denial{Tool: d.Tool, Detail: detail, Path: path})
	}
	if r.IsError {
		msg := strings.Join(r.Errors, "; ")
		if msg == "" {
			msg = r.Result
		}
		if msg == "" {
			msg = "Claude Code reported an error"
		}
		ev.Err = errors.New(msg)
	}
	return ev
}

// toolDetail picks the human-readable argument of a tool call, and the
// file it changes for edit tools.
func toolDetail(tool string, input json.RawMessage) (detail, path string) {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		path = str("file_path")
		if path == "" {
			path = str("notebook_path")
		}
		return path, path
	case "Bash", "PowerShell":
		return str("command"), ""
	}
	for _, k := range []string{"file_path", "path", "pattern", "url", "query", "description"} {
		if s := str(k); s != "" {
			return s, ""
		}
	}
	return "", ""
}

// processErr explains an agent that exited without a usable result.
func processErr(name string, exitErr error, stderr string) error {
	if line := lastLine(stderr); line != "" {
		return fmt.Errorf("%s: %s", name, line)
	}
	if exitErr != nil {
		return fmt.Errorf("%s: %w", name, exitErr)
	}
	return fmt.Errorf("%s ended without a reply", name)
}
