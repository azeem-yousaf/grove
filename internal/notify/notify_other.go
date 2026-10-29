//go:build !windows && !darwin

package notify

import (
	"errors"
	"os/exec"
)

// Send shows a notification with notify-send, which most Linux desktops
// have (libnotify).
func Send(title, body string) error {
	if _, err := exec.LookPath("notify-send"); err != nil {
		return errors.New("notify-send isn't installed")
	}
	return command("notify-send", "--app-name=grove", title, body).Run()
}
