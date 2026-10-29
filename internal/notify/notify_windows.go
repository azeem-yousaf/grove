package notify

import (
	"encoding/base64"
	"fmt"
	"strings"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// toastScript shows a toast under Windows PowerShell's app ID, which every
// Windows install has registered. The text comes from environment variables
// so nothing in it is ever parsed as PowerShell. It needs Windows
// PowerShell (powershell.exe): PowerShell 7 can't load WinRT types.
const toastScript = `$ErrorActionPreference = 'Stop'
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] > $null
$e = { param($s) [Security.SecurityElement]::Escape($s) }
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml("<toast><visual><binding template=""ToastGeneric""><text>$(& $e $env:GROVE_NOTIFY_TITLE)</text><text>$(& $e $env:GROVE_NOTIFY_BODY)</text></binding></visual></toast>")
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe').Show((New-Object Windows.UI.Notifications.ToastNotification $xml))
`

// Send shows a Windows toast notification.
func Send(title, body string) error {
	cmd := command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", encode(toastScript))
	cmd.Env = append(cmd.Environ(), "GROVE_NOTIFY_TITLE="+title, "GROVE_NOTIFY_BODY="+body)
	// No console window flashing up for it.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("showing a notification: %w: %s", err, firstLine(string(out)))
	}
	return nil
}

// encode is PowerShell's -EncodedCommand format: base64 of UTF-16LE.
func encode(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func firstLine(s string) string {
	for l := range strings.SplitSeq(s, "\n") {
		// Output sent to a pipe comes as CLIXML; skip its header.
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#<") {
			return l
		}
	}
	return ""
}
