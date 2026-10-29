package shell

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
)

// command runs line with cmd.exe, passing it through untouched: Go's own
// argument quoting doesn't follow cmd.exe's rules. When ctx ends, taskkill
// ends the whole process tree, since killing cmd.exe alone would leave the
// test runner going.
func command(ctx context.Context, line string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       `cmd.exe /d /s /c "` + line + `"`,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	cmd.Cancel = func() error {
		kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		if err := kill.Run(); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	return cmd
}
