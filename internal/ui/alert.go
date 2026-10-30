package ui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// While grove's terminal isn't focused, what the user would want to come
// back for (an agent finished, needs them or failed, a plan is ready or
// done, worktrees started to conflict) also goes out as a desktop
// notification, and as the terminal bell if [notify] bell is set. Alerts
// that arrive within alertDelay of the first go out together, so a plan's
// agents finishing at once make one notification, not a pile of them.
// While the terminal is focused, the toasts are enough.

const (
	alertDelay = 2 * time.Second
	alertLines = 4 // lines in one notification before "and N more"
)

type (
	alertFlushMsg struct{}
	alertSentMsg  struct{ err error }
)

// alert queues text for the next notification if the terminal isn't
// focused.
func (m *Model) alert(text string) tea.Cmd {
	n := m.cfg.Notify
	if m.termFocused || (!n.DesktopOn() && !n.Bell) {
		return nil
	}
	if !slices.Contains(m.alerts, text) {
		m.alerts = append(m.alerts, text)
	}
	if m.alertWaiting {
		return nil
	}
	m.alertWaiting = true
	return tick(alertDelay, func(time.Time) tea.Msg { return alertFlushMsg{} })
}

// flushAlerts sends what alert queued, unless the user has come back.
func (m *Model) flushAlerts() tea.Cmd {
	texts := m.alerts
	m.alerts, m.alertWaiting = nil, false
	if m.termFocused || len(texts) == 0 {
		return nil
	}
	var cmds []tea.Cmd
	if m.cfg.Notify.Bell {
		cmds = append(cmds, tea.Raw("\a"))
	}
	if m.cfg.Notify.DesktopOn() && !m.alertsBroken {
		title, body := alertText(m.repo.Name, texts)
		send := m.sendAlert
		cmds = append(cmds, func() tea.Msg { return alertSentMsg{send(title, body)} })
	}
	return tea.Batch(cmds...)
}

// alertText is the title and body of a notification for texts.
func alertText(repo string, texts []string) (title, body string) {
	title = "grove · " + repo
	if len(texts) == 1 {
		return title, texts[0]
	}
	title += fmt.Sprintf(" · %d updates", len(texts))
	if len(texts) > alertLines {
		more := len(texts) - (alertLines - 1)
		texts = append(texts[:alertLines-1:alertLines-1], fmt.Sprintf("and %d more", more))
	}
	return title, strings.Join(texts, "\n")
}

// onAlertSent stops trying after a notification fails, saying why once.
func (m *Model) onAlertSent(msg alertSentMsg) tea.Cmd {
	if msg.err == nil || m.alertsBroken {
		return nil
	}
	m.alertsBroken = true
	return m.notify("No desktop notifications: "+msg.err.Error()+"; set [notify] desktop = false to stop trying", toastErr)
}
