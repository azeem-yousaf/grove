package notify

// Send shows a notification through Notification Center. The text is passed
// as arguments, never inside the AppleScript itself.
func Send(title, body string) error {
	return command("osascript",
		"-e", "on run argv",
		"-e", "display notification (item 2 of argv) with title (item 1 of argv)",
		"-e", "end run",
		title, body).Run()
}
