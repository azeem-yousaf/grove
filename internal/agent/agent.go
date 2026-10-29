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
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/azeem-yousaf/grove/internal/usage"
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
	// Slot keeps a separate Claude process for one of several read-only
	// agents in the same folder: each group's planner beside the lead.
	Slot string
	// Commands lets a read-only agent run any shell command without asking:
	// the planner while it runs a plan the user started. Claude still can't
	// edit files with its own tools; Codex gets the workspace-write sandbox.
	Commands bool
	// Env is added to the agent process's environment, such as the git
	// guard's settings. A kept Claude process started with other values is
	// replaced (resuming its conversation).
	Env []string
	// Hook is the command line (`grove agent-hook ...`) Claude Code runs
	// before each shell command, which allows it, asks for it or leaves it
	// to the normal permissions. Codex has no hooks.
	Hook []string

	// Tools are MCP servers the agent's CLI starts for it, such as a
	// grouped task's group chat.
	Tools []ToolServer
	// ReadDirs are folders outside Dir the agent may read but not change:
	// a grouped task's groupmates' worktrees (Claude). Codex's sandbox
	// already lets it read anywhere.
	ReadDirs []string
	// SessionTotal is the last EvDone.SessionTotal of Session (Claude), for
	// when this Runner hasn't run it yet; zero if unknown. Without it, a
	// resumed session's turn is counted from result.usage alone, as Partial.
	// The caller must clear it when a turn's EvDone has none (a turn cut
	// short still adds to the session, so the old total would count it
	// twice); this Runner ignores it for a session in that state anyway.
	SessionTotal usage.Tokens
}

// ToolServer is an MCP server run over stdio by the agent's CLI.
type ToolServer struct {
	Name    string
	Command string
	Args    []string
	Allow   []string // its tools the agent may call without asking
}

// procKey is the kept Claude process for req: one per folder, plus one for
// each read-only agent there (by Slot).
func procKey(req Request) string {
	if req.ReadOnly {
		return req.Dir + "\x00read-only" + req.Slot
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
	EvDone                     // the turn ended: Denied, Cost, Tokens, TokenStatus, Err
	// EvUsage: Tokens is what the turn has used so far, replacing the last
	// EvUsage (a running total, not a delta). Sent only when it changes.
	EvUsage
	// EvOutput: a shell command the agent ran (Tool "Bash", "PowerShell" or
	// "Run", Detail the command) finished; Text is the end of its output,
	// Failed whether it failed.
	EvOutput
)

// Event is something the agent did.
type Event struct {
	Kind    EventKind
	Session string
	Text    string
	Tool    string // "Edit", "Bash", "Read", ...
	Detail  string // the command, file or pattern
	Path    string // file being changed, for edits
	Failed  bool   // EvOutput: the command failed
	Denied  []Denial
	Cost    string // "$0.04" (Claude Code)
	// Tokens is the turn's usage: so far for EvUsage, all of it for EvDone.
	Tokens usage.Tokens
	// TokenStatus says how far EvDone's Tokens can be trusted: Exact is the
	// provider's own figure for the turn (also after an error or a stop),
	// Partial what was counted before the turn was cut short, Unknown when
	// nothing usable was reported. Figures are never estimated.
	TokenStatus usage.Status
	// SessionTotal is, on a Claude EvDone, everything the session has used
	// since it began (Claude Code's modelUsage, which survives --resume).
	// Pass it back as Request.SessionTotal after grove restarts so the next
	// turn's figure can be Exact. Zero when unknown, which makes any total
	// saved before it stale.
	SessionTotal usage.Tokens
	Err          error
}

// outputTail is how much of a command's output an EvOutput keeps.
const outputTail = 4000

// tail is the end of a command's output, at most outputTail bytes, starting
// on a whole line when it was cut.
func tail(out string) string {
	out = strings.TrimRight(strings.ToValidUTF8(out, "?"), "\r\n\t ")
	if len(out) <= outputTail {
		return out
	}
	out = out[len(out)-outputTail:]
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = out[i+1:]
	}
	return strings.ToValidUTF8(out, "")
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

	// Replay limits the history a Codex turn sends; zero means DefaultReplay.
	Replay Replay

	mu    sync.Mutex
	procs  map[string]*claudeProc   // by Request.Dir
	totals map[string]*usage.Tokens // each Claude session's SessionTotal by session id; nil once unknown
}

func NewRunner() *Runner {
	return &Runner{Idle: 10 * time.Minute, Replay: DefaultReplay, procs: map[string]*claudeProc{}, totals: map[string]*usage.Tokens{}}
}

// sessionBase is what req's session had used before this turn: nothing for
// a new session, else the last total seen here or passed in. nil if
// unknown, which includes a session whose last turn here ended without a
// total: Claude Code still added that turn to the session, so any earlier
// total, the request's included, would count it again.
func (r *Runner) sessionBase(req Request) *usage.Tokens {
	if req.Session == "" {
		return &usage.Tokens{}
	}
	r.mu.Lock()
	t, ok := r.totals[req.Session]
	r.mu.Unlock()
	switch {
	case ok && t == nil:
		return nil
	case ok:
		base := *t
		return &base
	case req.SessionTotal.IsZero():
		return nil
	}
	base := req.SessionTotal
	return &base
}

// noteTotal records the session's total after a turn, or that it is no
// longer known.
func (r *Runner) noteTotal(req Request, ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !ev.SessionTotal.IsZero() && ev.Session != "" {
		t := ev.SessionTotal
		r.totals[ev.Session] = &t
		return
	}
	for _, s := range []string{req.Session, ev.Session} {
		if s != "" {
			r.totals[s] = nil
		}
	}
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
		ev := p.turn(ctx, req, r.sessionBase(req), emit)
		r.release(procKey(req), p)
		r.noteTotal(req, ev)
		emit(ev)
	case Codex:
		args, input := codexArgs(req, r.Replay)
		runOnce(ctx, bin, req.Dir, req.Env, args, input, &codexParser{}, emit)
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
func runOnce(ctx context.Context, bin, dir string, env, args []string, input string, p parser, emit func(Event)) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = environ(env)
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

// environ is grove's environment with extra appended, or nil (inherit it)
// when there is nothing extra. exec keeps the last value of a repeated
// name, so extra wins.
func environ(extra []string) []string {
	if len(extra) == 0 {
		return nil
	}
	return append(os.Environ(), extra...)
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
