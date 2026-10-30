// Package shell runs a command line the user wrote, such as a test command
// from the config, through the system's shell: sh on macOS and Linux,
// cmd.exe on Windows. A command that runs past its context is killed along
// with everything it started.
package shell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Result is how a command went.
type Result struct {
	ExitCode int    // -1 when it didn't start or was killed
	Output   []byte // stdout and stderr together, at most the last limit bytes
	TimedOut bool
	Err      error // it couldn't run, or was killed; a non-zero exit isn't one
}

// OK reports whether the command ran and exited 0.
func (r Result) OK() bool { return r.Err == nil && r.ExitCode == 0 }

// Run runs line in dir and keeps the last limit bytes of its output. Its
// stdin is empty and CI=true is set, so test runners don't wait for input
// or start watching files.
func Run(ctx context.Context, dir, line string, limit int) Result {
	return RunEnv(ctx, dir, line, limit, nil)
}

// RunEnv is Run with env added to the environment, after grove's own (for
// example the git guard's settings).
func RunEnv(ctx context.Context, dir, line string, limit int, env []string) Result {
	cmd := command(ctx, line)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "CI=true"), env...)
	out := &tailBuffer{limit: limit}
	cmd.Stdout, cmd.Stderr = out, out
	// Pipes held open by a grandchild must not keep Wait waiting.
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	r := Result{ExitCode: -1, Output: out.bytes()}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		r.TimedOut, r.Err = errors.Is(ctx.Err(), context.DeadlineExceeded), ctx.Err()
	case errors.As(err, &exit):
		r.ExitCode = exit.ExitCode()
	case err != nil:
		r.Err = err
	default:
		r.ExitCode = 0
	}
	return r
}

// tailBuffer keeps the last limit bytes written to it.
type tailBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if over := t.buf.Len() - t.limit; t.limit > 0 && over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Clone(t.buf.Bytes())
}
