package ui

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
)

type sentAlert struct{ title, body string }

// alertModel is fakeModel with notifications recorded instead of shown.
func alertModel(t *testing.T) (*Model, *[]sentAlert) {
	t.Helper()
	m := fakeModel(t, 140, 36)
	var sent []sentAlert
	m.sendAlert = func(title, body string) error {
		sent = append(sent, sentAlert{title, body})
		return nil
	}
	return m, &sent
}

// runCmd runs cmd and what it batches, feeding alertSentMsgs back into m.
// Ticks are left alone: tests send alertFlushMsg themselves.
func runCmd(m *Model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			runCmd(m, c)
		}
	case alertSentMsg:
		m.Update(msg)
	}
}

// finishTurn ends a turn of the agent in path, as if it had been working.
func finishTurn(m *Model, path, name string, ev agent.Event) {
	s := m.sessionFor(path)
	if s == nil {
		s = m.newSession(path, name, agent.Claude)
	}
	s.running = true
	ev.Kind = agent.EvDone
	m.Update(agentMsg{path: path, ev: ev})
}

func flush(m *Model) {
	_, cmd := m.Update(alertFlushMsg{})
	runCmd(m, cmd)
}

func TestAlertWhileAway(t *testing.T) {
	m, sent := alertModel(t)
	auth, cart := filepath.FromSlash("/src/shop-auth"), filepath.FromSlash("/src/shop-cart")

	// Focused: the toast is enough.
	finishTurn(m, auth, "feature/auth", agent.Event{})
	flush(m)
	if len(*sent) != 0 || len(m.alerts) != 0 {
		t.Fatalf("focused terminal sent %+v (queued %v)", *sent, m.alerts)
	}

	m.Update(tea.BlurMsg{})
	finishTurn(m, auth, "feature/auth", agent.Event{})
	if !m.alertWaiting {
		t.Fatal("no flush scheduled")
	}
	flush(m)
	want := sentAlert{"grove · shop", "feature/auth: Claude Code finished"}
	if len(*sent) != 1 || (*sent)[0] != want {
		t.Fatalf("sent %+v, want %+v", *sent, want)
	}

	// Alerts close together make one notification.
	*sent = nil
	finishTurn(m, auth, "feature/auth", agent.Event{Err: errors.New("rate limited")})
	finishTurn(m, cart, "feature/cart", agent.Event{Denied: []agent.Denial{{Tool: "Bash", Detail: "npm test"}}})
	flush(m)
	if len(*sent) != 1 || (*sent)[0].title != "grove · shop · 2 updates" ||
		(*sent)[0].body != "feature/auth: rate limited\nfeature/cart needs you" {
		t.Fatalf("batched: %+v", *sent)
	}

	// Coming back before the flush drops what was queued.
	*sent = nil
	finishTurn(m, auth, "feature/auth", agent.Event{})
	m.Update(tea.FocusMsg{})
	flush(m)
	if len(*sent) != 0 {
		t.Fatalf("sent after coming back: %+v", *sent)
	}
}

func TestAlertFailsOnce(t *testing.T) {
	m, _ := alertModel(t)
	calls := 0
	m.sendAlert = func(string, string) error {
		calls++
		return errors.New("no notifier")
	}
	auth := filepath.FromSlash("/src/shop-auth")
	m.Update(tea.BlurMsg{})
	finishTurn(m, auth, "feature/auth", agent.Event{})
	flush(m)
	if !strings.Contains(m.toast.text, "no notifier") {
		t.Errorf("toast = %q", m.toast.text)
	}
	finishTurn(m, auth, "feature/auth", agent.Event{})
	flush(m)
	if calls != 1 {
		t.Errorf("tried %d times after failing", calls)
	}
}

func TestAlertConfig(t *testing.T) {
	m, sent := alertModel(t)
	off := false
	m.cfg.Notify.Desktop = &off
	m.Update(tea.BlurMsg{})
	finishTurn(m, filepath.FromSlash("/src/shop-auth"), "feature/auth", agent.Event{})
	if m.alertWaiting {
		t.Error("alert queued with notifications off")
	}

	m.cfg.Notify.Bell = true
	finishTurn(m, filepath.FromSlash("/src/shop-auth"), "feature/auth", agent.Event{})
	_, cmd := m.Update(alertFlushMsg{})
	if cmd == nil {
		t.Fatal("no bell")
	}
	runCmd(m, cmd)
	if len(*sent) != 0 {
		t.Errorf("desktop notification while off: %+v", *sent)
	}
}

func TestAlertText(t *testing.T) {
	texts := []string{"a", "b", "c", "d", "e", "f"}
	title, body := alertText("shop", texts)
	if title != "grove · shop · 6 updates" || body != "a\nb\nc\nand 3 more" {
		t.Errorf("got %q / %q", title, body)
	}
	if len(texts) != 6 || texts[3] != "d" {
		t.Errorf("texts changed: %v", texts)
	}
}
