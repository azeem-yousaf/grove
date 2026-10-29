// Package notify shows desktop notifications, so grove can tell the user
// about agents while they are working in another window.
//
// Send is implemented per OS: a Windows toast through Windows PowerShell,
// osascript on macOS and notify-send elsewhere. Each runs a short-lived
// program and returns once it has handed the notification to the system.
package notify

import (
	"os"
	"os/exec"
)

// command prepares a helper program. It runs from the temporary folder
// rather than grove's own working directory, which may be a worktree the
// user is about to remove; Windows can't delete a folder a process is in.
func command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = os.TempDir()
	return cmd
}
