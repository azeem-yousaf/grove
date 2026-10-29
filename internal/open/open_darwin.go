package open

import (
	"os/exec"
	"strings"
	"syscall"
)

const fileManager = "Finder"

// Default opens path with its default application.
func Default(path string) error { return start(exec.Command("open", path)) }

// URL opens a web address in the default browser.
func URL(url string) error { return Default(url) }

// Reveal shows path selected in Finder.
func Reveal(path string) error { return start(exec.Command("open", "-R", path)) }

func start(cmd *exec.Cmd) error {
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// Copy puts text on the clipboard.
func Copy(text string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
