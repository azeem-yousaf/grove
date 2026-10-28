// Package agent drives a command-line AI agent that is already installed
// and signed in (Claude Code or Codex) inside one worktree. grove never
// handles API keys; the agent's own login and plan are used.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Kind is a supported agent.
type Kind string

const (
	Claude Kind = "claude"
	Codex  Kind = "codex"
)

// Kinds lists the supported agents in the order they're offered.
var Kinds = []Kind{Claude, Codex}

// Label is the product name.
func (k Kind) Label() string {
	switch k {
	case Claude:
		return "Claude Code"
	case Codex:
		return "Codex"
	}
	return string(k)
}

// Parse accepts "claude" or "codex".
func Parse(s string) (Kind, bool) {
	for _, k := range Kinds {
		if strings.EqualFold(strings.TrimSpace(s), string(k)) {
			return k, true
		}
	}
	return "", false
}

var lookPath = exec.LookPath

// Find returns the agent's executable, or "" if it isn't installed. On
// Windows a native .exe is preferred over npm's .cmd shims, which pass
// arguments through cmd.exe.
func Find(k Kind) string {
	if runtime.GOOS == "windows" {
		if p, err := lookPath(string(k) + ".exe"); err == nil {
			return p
		}
	}
	if p, err := lookPath(string(k)); err == nil {
		return p
	}
	return ""
}

// Installed returns the agents found on PATH.
func Installed() []Kind {
	var out []Kind
	for _, k := range Kinds {
		if Find(k) != "" {
			out = append(out, k)
		}
	}
	return out
}

// Turn is one earlier exchange, replayed for agents without resume.
type Turn struct{ User, Agent string }

// Request is one turn of a conversation.
type Request struct {
	Kind    Kind
	Bin     string // executable; Find(Kind) when empty
	Dir     string // the worktree, used as the working folder
	Message string // what the user asked this turn
	// Context is grove's instructions and a summary of the worktree. It is
	// sent through stdin so its size and characters never hit the command
	// line.
	Context string
	History []Turn   // earlier turns (Codex)
	Session string   // session to resume (Claude)
	Allow   []string // extra permission rules the user approved (Claude)
	Model   string
	// ReadOnly lets the agent read but not change files: the planner.
	// Claude refuses edits and may use read-only Jira tools; Codex runs in
	// its read-only sandbox. It gets its own process even in a folder that
	// also has a worktree agent.
	ReadOnly bool
	// Tools are MCP servers the agent's CLI starts for it, such as a
	// grouped task's group chat.
	Tools []ToolServer
	// ReadDirs are folders outside Dir the agent may read but not change:
	// a grouped task's groupmates' worktrees (Claude). Codex's sandbox
	// already lets it read anywhere.
	ReadDirs []string
}

// ToolServer is an MCP server run over stdio by the agent's CLI.
type ToolServer struct {
	Name    string
	Command string
	Args    []string
	Allow   []string // its tools the agent may call without asking
}

// procKey is the kept Claude process for req: one per folder, plus one for
// a read-only agent there.
func procKey(req Request) string {
	if req.ReadOnly {
		return req.Dir + "\x00read-only"
	}
	return req.Dir
}

// EventKind classifies an Event.
type EventKind int

const (
	EvSession EventKind = iota // Session: the id to resume with
	EvDelta                    // Text: more of the reply as it streams
	EvText                     // Text: a complete message
	EvTool                     // Tool, Detail, Path: the agent is doing something
	EvDone                     // the turn ended: Denied, Cost, Err
)

// Event is something the agent did.
type Event struct {
	Kind    EventKind
	Session string
	Text    string
	Tool    string // "Edit", "Bash", "Read", ...
	Detail  string // the command, file or pattern
	Path    string // file being changed, for edits
	Denied  []Denial
	Cost    string // "$0.04" or "12.3k tokens"
	Err     error
}

// Denial is a tool call the agent wasn't allowed to make.
type Denial struct {
	Tool   string
	Detail string // the command for Bash, the file otherwise
	Path   string
}

// parser turns one agent's output lines into events.
type parser interface {
	line(b []byte) []Event
	// done builds the final event once the process has exited.
	done(exitErr error, stderr string) Event
}

var errStopped = errors.New("stopped")

// Runner runs agent turns. Claude Code keeps one process per worktree
// between turns, so its startup (settings, hooks, MCP servers) is paid once
// per conversation rather than once per message; Codex starts fresh every
// turn. A kept process exits when grove does, because its input closes.
type Runner struct {
	// Idle is how long a Claude process with nothing to do is kept.
	Idle time.Duration

	mu    sync.Mutex
	procs map[string]*claudeProc // by Request.Dir
}

func NewRunner() *Runner {
	return &Runner{Idle: 10 * time.Minute, procs: map[string]*claudeProc{}}
}

func binFor(req Request) string {
	if req.Bin != "" {
		return req.Bin
	}
	return Find(req.Kind)
}

// Run runs one turn and reports what the agent does through emit, ending
// with exactly one EvDone. Cancelling ctx stops the turn.
func (r *Runner) Run(ctx context.Context, req Request, emit func(Event)) {
	bin := binFor(req)
	if bin == "" {
		emit(Event{Kind: EvDone, Err: fmt.Errorf("%s isn't installed or isn't on PATH", req.Kind.Label())})
		return
	}
	switch req.Kind {
	case Claude:
		p, err := r.claim(bin, req)
		if err != nil {
			emit(Event{Kind: EvDone, Err: err})
			return
		}
		ev := p.turn(ctx, req, emit)
		r.release(procKey(req), p)
		emit(ev)
	case Codex:
		args, input := codexArgs(req)
		runOnce(ctx, bin, req.Dir, args, input, &codexParser{}, emit)
	default:
		emit(Event{Kind: EvDone, Err: fmt.Errorf("unknown agent %q", req.Kind)})
	}
}

// Warm starts Claude for req's next turn ahead of time, so that turn
// doesn't wait for it to start. Other agents have nothing to warm up.
func (r *Runner) Warm(req Request) {
	bin := binFor(req)
	if req.Kind != Claude || bin == "" {
		return
	}
	key := procKey(req)
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.procs[key]; p != nil {
		if p.busy || p.fits(bin, req) {
			return
		}
		p.end()
	}
	p, err := startClaude(bin, req)
	if err != nil {
		delete(r.procs, key)
		return // the turn will start it again and report the error
	}
	r.procs[key] = p
	r.idleLater(key, p)
}

// Stop ends the agent kept for dir and waits for it to exit, so the folder
// is no longer in use.
func (r *Runner) Stop(dir string) {
	r.mu.Lock()
	p := r.procs[dir]
	delete(r.procs, dir)
	r.mu.Unlock()
	if p != nil {
		p.end()
		<-p.exited
	}
}

// claim returns a process ready for req's turn, reusing the kept one when
// it was started the same way and is in the same conversation.
func (r *Runner) claim(bin string, req Request) (*claudeProc, error) {
	key := procKey(req)
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.procs[key]; p != nil {
		if !p.busy && p.fits(bin, req) {
			p.busy = true
			if p.idle != nil {
				p.idle.Stop()
			}
			return p, nil
		}
		p.end()
		delete(r.procs, key)
	}
	p, err := startClaude(bin, req)
	if err != nil {
		return nil, err
	}
	p.busy = true
	r.procs[key] = p
	return p, nil
}

// release keeps p for the next turn, or forgets it if it has exited. key
// is its procKey.
func (r *Runner) release(dir string, p *claudeProc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p.busy = false
	if r.procs[dir] != p {
		return
	}
	if p.ended() {
		delete(r.procs, dir)
		return
	}
	r.idleLater(dir, p)
}

// idleLater ends p once it has had nothing to do for r.Idle. Callers hold
// r.mu.
func (r *Runner) idleLater(dir string, p *claudeProc) {
	p.idle = time.AfterFunc(r.Idle, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.procs[dir] == p && !p.busy {
			delete(r.procs, dir)
			p.end()
		}
	})
}

// runOnce runs an agent process for a single turn.
func runOnce(ctx context.Context, bin, dir string, args []string, input string, p parser, emit func(Event)) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input) // closed at EOF, so the agent doesn't wait for more
	var stderr tailBuffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		emit(Event{Kind: EvDone, Err: err})
		return
	}
	if err := cmd.Start(); err != nil {
		emit(Event{Kind: EvDone, Err: err})
		return
	}
	r := bufio.NewReaderSize(out, 64*1024)
	for {
		b, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(b)) > 0 {
			for _, ev := range p.line(b) {
				emit(ev)
			}
		}
		if err != nil {
			break
		}
	}
	err = cmd.Wait()
	if ctx.Err() != nil {
		err = errStopped
	}
	emit(p.done(err, stderr.String()))
}

// tailBuffer keeps the last few KB written to it, for error messages.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 8192 {
		t.b = t.b[len(t.b)-8192:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

// lastLine is the most useful line of an error stream.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

var _ io.Writer = (*tailBuffer)(nil)
