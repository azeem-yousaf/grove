// Package git is grove's window into git. Everything goes through the real git
// CLI so grove always agrees with what `git` itself reports.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// ErrNotRepo is returned when a path is not inside a git repository.
var ErrNotRepo = errors.New("not a git repository")

// Error is a failed git invocation with its stderr attached.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), e.Stderr)
	}
	return fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// run executes git in dir and returns stdout.
func run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return runCodes(ctx, dir, nil, args...)
}

// runCodes is run, but treats the given exit codes as success. `git diff
// --no-index` exits 1 when files differ, for example.
func runCodes(ctx context.Context, dir string, okCodes []int, args ...string) ([]byte, error) {
	return runWith(ctx, dir, call{ok: okCodes}, args...)
}

// call holds the less common options for running git.
type call struct {
	ok    []int     // exit codes that count as success
	env   []string  // extra environment, e.g. GIT_INDEX_FILE=...
	stdin io.Reader // input for --stdin / --pathspec-from-file=-
}

func runWith(ctx context.Context, dir string, c call, args ...string) ([]byte, error) {
	okCodes := c.ok
	full := append([]string{
		"-C", dir,
		"--no-pager",
		"-c", "core.quotepath=off",
		"-c", "color.ui=never",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	// GIT_OPTIONAL_LOCKS=0 stops `git status` from refreshing the index, so
	// grove polling in the background never fights with the user's own git
	// commands over index.lock.
	cmd.Env = append(os.Environ(),
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	cmd.Env = append(cmd.Env, c.env...)
	cmd.Stdin = c.stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && slices.Contains(okCodes, ee.ExitCode()) {
			return stdout.Bytes(), nil
		}
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "not a git repository") {
			return nil, ErrNotRepo
		}
		return nil, &Error{Args: args, Stderr: firstLine(msg), Err: err}
	}
	return stdout.Bytes(), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return strings.TrimPrefix(strings.TrimSpace(s), "fatal: ")
}

// splitNUL splits -z output into fields, dropping the trailing terminator.
func splitNUL(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// nativePath converts a path printed by git (always forward slashes) to the
// platform's form.
func nativePath(p string) string { return filepath.Clean(filepath.FromSlash(p)) }

// Available reports whether a git binary is on PATH.
func Available() error {
	if _, err := exec.LookPath("git"); err != nil {
		return errors.New("git was not found on PATH")
	}
	return nil
}
