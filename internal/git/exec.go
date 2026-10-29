package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
)

// Exec runs git with args in dir for an agent or the user, the way they
// would have typed it, with env added to grove's own environment (the git
// guard's, for example). Unlike run, it returns stdout and stderr together,
// as a terminal would show them, and the exit code (-1 if git didn't run).
func Exec(ctx context.Context, dir string, args, env []string) (out []byte, code int, err error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true")
	cmd.Env = append(cmd.Env, env...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err = cmd.Run()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		return buf.Bytes(), ee.ExitCode(), err
	}
	if err != nil {
		return buf.Bytes(), -1, err
	}
	return buf.Bytes(), 0, nil
}

// Push pushes branch from dir to the same name on remote, setting it as the
// branch's upstream, with env added (the git guard's).
func Push(ctx context.Context, dir, remote, branch string, env []string) error {
	_, err := runWith(ctx, dir, call{env: env}, "push", "--porcelain", "-u", remote, "refs/heads/"+branch+":refs/heads/"+branch)
	return err
}

// RefHash is the commit ref points at in dir, or "" when it doesn't exist.
func RefHash(ctx context.Context, dir, ref string) string {
	out, err := run(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(out))
}
