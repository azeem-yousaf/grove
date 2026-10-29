//go:build !windows

package shell

import (
	"context"
	"os/exec"
	"syscall"
)

// command runs line with sh in its own process group, which is killed as
// a whole when ctx ends.
func command(ctx context.Context, line string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}
