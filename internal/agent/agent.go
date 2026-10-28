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

// Run starts the agent for one turn and reports what it does through emit,
// ending with exactly one EvDone. Cancelling ctx stops the agent.
func Run(ctx context.Context, req Request, emit func(Event)) {
	bin := req.Bin
	if bin == "" {
		bin = Find(req.Kind)
	}
	if bin == "" {
		emit(Event{Kind: EvDone, Err: fmt.Errorf("%s isn't installed or isn't on PATH", req.Kind.Label())})
		return
	}
	var (
		args  []string
		input string
		p     parser
	)
	switch req.Kind {
	case Claude:
		args, input = claudeArgs(req)
		p = &claudeParser{}
	case Codex:
		args, input = codexArgs(req)
		p = &codexParser{}
	default:
		emit(Event{Kind: EvDone, Err: fmt.Errorf("unknown agent %q", req.Kind)})
		return
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = req.Dir
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
		err = errors.New("stopped")
	}
	emit(p.done(err, stderr.String()))
}

// tailBuffer keeps the last few KB written to it, for error messages.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 8192 {
		t.b = t.b[len(t.b)-8192:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return strings.TrimSpace(string(t.b)) }

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
