package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// chatSelection is a range of conversation lines picked by dragging the
// mouse. grove takes the mouse for clicks and scrolling, so the terminal's
// own selection would also pick up the file list and the borders.
type chatSelection struct {
	active   bool
	dragging bool
	from, to int // line indexes into the conversation, in drag order
}

func (c chatSelection) bounds() (int, int) {
	return min(c.from, c.to), max(c.from, c.to)
}

func (c chatSelection) has(i int) bool {
	lo, hi := c.bounds()
	return c.active && i >= lo && i <= hi
}

// chatGutter is the width of the speaker column before each message.
const chatGutter = 6

// chatArea is where the conversation lines are on screen.
func (m *Model) chatArea() (x, y, w, h int) {
	listW, diffW, _ := m.layout()
	x = listW + 1
	if m.narrow() {
		x = 1
	}
	return x, 2, diffW - 2, m.chatView.Height() // below the header and the pane's top border
}

// chatLineAt is the conversation line under a screen row, clamped to the
// lines on screen.
func (m *Model) chatLineAt(y int) int {
	_, top, _, h := m.chatArea()
	row := max(0, min(y-top, h-1))
	return max(0, min(m.chatView.YOffset()+row, m.chatView.TotalLineCount()-1))
}

// inChat reports whether a screen cell is over the conversation.
func (m *Model) inChat(x, y int) bool {
	if m.focus != focusChat || m.chatSession() == nil {
		return false
	}
	cx, cy, w, h := m.chatArea()
	return x >= cx && x < cx+w && y >= cy && y < cy+h
}

func (m *Model) startChatSelection(y int) {
	i := m.chatLineAt(y)
	m.chatSel = chatSelection{active: true, dragging: true, from: i, to: i}
}

func (m *Model) onMouseMotion(msg tea.MouseMotionMsg) tea.Cmd {
	if !m.chatSel.dragging {
		return nil
	}
	// Dragging past the edge scrolls, so long replies can be picked.
	_, top, _, h := m.chatArea()
	switch {
	case msg.Y < top:
		m.chatView.ScrollUp(1)
	case msg.Y >= top+h:
		m.chatView.ScrollDown(1)
	}
	m.chatFollow = false
	m.chatSel.to = m.chatLineAt(msg.Y)
	return nil
}

func (m *Model) onMouseRelease(tea.MouseReleaseMsg) tea.Cmd {
	if !m.chatSel.dragging {
		return nil
	}
	m.chatSel.dragging = false
	if m.chatSel.from == m.chatSel.to {
		// A click, not a drag.
		m.chatSel = chatSelection{}
		return nil
	}
	s := m.chatSession()
	if s == nil {
		return nil
	}
	_, _, w, _ := m.chatArea()
	lo, hi := m.chatSel.bounds()
	lines := m.chatLines(s, w)
	text := selectedChatText(lines[min(lo, len(lines)):min(hi+1, len(lines))])
	if text == "" {
		m.chatSel = chatSelection{}
		return nil
	}
	return m.copyText(text, "Copied "+plural(hi-lo+1, "line"))
}

// selectedChatText turns rendered conversation lines back into plain text:
// colours, the speaker column and trailing padding go, and wrapped code
// lines are joined back into one.
func selectedChatText(lines []string) string {
	var out []string
	for _, l := range lines {
		plain := ansi.Strip(l)
		if ansi.StringWidth(plain) > chatGutter {
			plain = ansi.Cut(plain, chatGutter, ansi.StringWidth(plain))
		} else {
			plain = ""
		}
		plain = strings.TrimRight(plain, " ")
		if rest, ok := strings.CutPrefix(plain, codeWrap); ok && len(out) > 0 {
			out[len(out)-1] += " " + strings.TrimLeft(rest, " ")
			continue
		}
		out = append(out, plain)
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// highlightChat marks the selected lines.
func (m *Model) highlightChat(lines []string, width int) []string {
	if !m.chatSel.active {
		return lines
	}
	lo, hi := m.chatSel.bounds()
	out := append([]string(nil), lines...)
	for i := max(lo, 0); i <= hi && i < len(out); i++ {
		out[i] = fill(m.st.text.Render(ansi.Strip(out[i])), width, m.st.p.selBg)
	}
	return out
}
