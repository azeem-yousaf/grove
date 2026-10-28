//go:build !windows && !darwin

package open

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const fileManager = "file manager"

// Default opens path with the desktop's default application.
func Default(path string) error { return start(exec.Command("xdg-open", path)) }

// Reveal opens the folder containing path. xdg-open has no portable way to
// select the file itself.
func Reveal(path string) error {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return Default(path)
	}
	return Default(filepath.Dir(path))
}

func start(cmd *exec.Cmd) error {
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// Copy is not supported natively; the UI falls back to OSC 52.
func Copy(string) error { return errors.ErrUnsupported }
