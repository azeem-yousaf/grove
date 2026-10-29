package notify

import "testing"

func TestEncode(t *testing.T) {
	// UTF-16LE, then base64: "A" is 41 00, "é" is E9 00.
	if got := encode("Aé"); got != "QQDpAA==" {
		t.Errorf("encode = %q", got)
	}
}

func TestFirstLine(t *testing.T) {
	out := "#< CLIXML\r\n\r\nCannot find type [Windows.UI.Notifications]\r\nmore"
	if got := firstLine(out); got != "Cannot find type [Windows.UI.Notifications]" {
		t.Errorf("firstLine = %q", got)
	}
}
