package open

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

const fileManager = "Explorer"

// Default opens path with the program Windows has associated with it. Files
// with no association get the "Open with" picker instead of an error.
func Default(path string) error {
	err := shellExecute("open", path)
	if errors.Is(err, windows.ERROR_NO_ASSOCIATION) {
		cmd := exec.Command("rundll32.exe", "shell32.dll,OpenAs_RunDLL", path)
		detach(cmd)
		return cmd.Start()
	}
	return err
}

// Reveal shows path selected in Explorer, or opens the folder when the file
// no longer exists.
func Reveal(path string) error {
	if _, err := os.Stat(path); err != nil {
		return Default(filepath.Dir(path))
	}
	// Explorer needs the quotes inside the /select, argument, which Go's
	// normal argument escaping won't produce.
	cmd := exec.Command("explorer.exe")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// URL opens a web address in the default browser.
func URL(url string) error {
	v, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	u, err := windows.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, v, u, nil, nil, windows.SW_SHOWNORMAL)
}

func shellExecute(verb, path string) error {
	v, err := windows.UTF16PtrFromString(verb)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	d, err := windows.UTF16PtrFromString(filepath.Dir(path))
	if err != nil {
		return err
	}
	return windows.ShellExecute(0, v, p, nil, d, windows.SW_SHOWNORMAL)
}

// detach starts the child in its own process group so it outlives grove and
// never receives grove's console signals.
func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
}

// Copy puts text on the Windows clipboard.
func Copy(text string) error {
	cmd := exec.Command("clip.exe")
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
