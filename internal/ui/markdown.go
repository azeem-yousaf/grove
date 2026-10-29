package ui

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var (
	// listItem is a Markdown list item: its indent, marker and text.
	listItem = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+(.*)$`)
	heading  = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
	hrule    = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})$`)
	quote    = regexp.MustCompile(`^\s*>\s?(.*)$`)
	// link is [text](target), with no spaces in the target.
	link = regexp.MustCompile(`^\[([^\]]+)\]\(([^)\s]+)\)`)
)

// renderProse wraps one line of Markdown prose to width. Headings are
// bold, quotes get a bar, list items get a bullet (or a box for a task)
// and a hanging indent so wrapped lines line up with their text, and
// inline marks are applied and dropped.
func renderProse(l string, width int, st styles, style lipgloss.Style) []string {
	trimmed := strings.TrimSpace(l)
	if hrule.MatchString(trimmed) {
		return []string{st.faint.Render(strings.Repeat("─", max(width, 1)))}
	}
	bar, text := "", strings.TrimRight(l, " ")
	if m := quote.FindStringSubmatch(text); m != nil {
		bar, text, style = "│ ", m[1], st.muted
		trimmed = strings.TrimSpace(text)
	}
	first := ""
	if m := heading.FindStringSubmatch(trimmed); m != nil {
		style, text = style.Bold(true).Foreground(st.p.accent), m[1]
	} else if m := listItem.FindStringSubmatch(text); m != nil {
		marker, item := m[2], m[3]
		if strings.ContainsAny(marker, "-*+") {
			marker = "•"
			switch {
			case strings.HasPrefix(item, "[ ] "):
				marker, item = "○", item[4:]
			case strings.HasPrefix(item, "[x] "), strings.HasPrefix(item, "[X] "):
				marker, item = "✓", item[4:]
			}
		}
		first, text = expandTabs(m[1])+marker+" ", item
	}
	hang := strings.Repeat(" ", ansi.StringWidth(first))
	wrapped := lipgloss.NewStyle().Width(max(width-ansi.StringWidth(bar)-len(hang), 10)).Render(inline(text, st, style))
	lines := strings.Split(wrapped, "\n")
	for i := range lines {
		p := hang
		if i == 0 {
			p = first
		}
		lines[i] = st.faint.Render(bar+p) + lines[i]
	}
	return lines
}

// inline styles Markdown code spans, bold, italics and links, dropping
// their marks. A mark with no partner is shown as it is.
func inline(s string, st styles, base lipgloss.Style) string {
	var b strings.Builder
	bold, italic, mark := false, false, byte(0)
	prev := ' ' // the character before s
	style := func(s lipgloss.Style) lipgloss.Style {
		return s.Bold(bold || s.GetBold()).Italic(italic)
	}
	for s != "" {
		switch {
		case s[0] == '`':
			if end := strings.IndexByte(s[1:], '`'); end > 0 {
				b.WriteString(style(st.info).Render(s[1 : end+1]))
				s, prev = s[end+2:], '`'
				continue
			}
		case strings.HasPrefix(s, "**") && (bold || strings.Contains(s[2:], "**")):
			bold = !bold
			s, prev = s[2:], '*'
			continue
		case s[0] == '[':
			if m := link.FindStringSubmatch(s); m != nil {
				b.WriteString(style(st.info).Underline(true).Render(m[1]))
				// A web address is worth seeing; a file path usually
				// repeats the text.
				if m[2] != m[1] && (strings.HasPrefix(m[2], "http://") || strings.HasPrefix(m[2], "https://")) {
					b.WriteString(st.faint.Render(" (" + m[2] + ")"))
				}
				s, prev = s[len(m[0]):], ')'
				continue
			}
		case italic && s[0] == mark && closesEmphasis(prev, s):
			italic = false
			s, prev = s[1:], rune(mark)
			continue
		case !italic && (s[0] == '*' || s[0] == '_') && opensEmphasis(prev, s):
			italic, mark = true, s[0]
			s, prev = s[1:], rune(mark)
			continue
		}
		// Plain text up to the next possible mark.
		run := s
		if n := strings.IndexAny(s[1:], "`*_["); n >= 0 {
			run = s[:n+1]
		}
		b.WriteString(style(base).Render(run))
		s = s[len(run):]
		prev, _ = utf8.DecodeLastRuneInString(run)
	}
	return b.String()
}

// opensEmphasis reports whether the * or _ starting s opens italics: it
// is followed by text, has a closing partner, and an _ isn't inside a
// word (snake_case).
func opensEmphasis(prev rune, s string) bool {
	mark := s[0]
	if len(s) < 3 || s[1] == ' ' || s[1] == mark || (mark == '_' && isWordRune(prev)) {
		return false
	}
	for i := 2; i < len(s); i++ {
		if s[i] == mark && closesEmphasis(rune(s[i-1]), s[i:]) {
			return true
		}
	}
	return false
}

// closesEmphasis reports whether the mark starting s closes italics: it
// follows text, isn't the start of a ** pair, and an _ isn't inside a word.
func closesEmphasis(prev rune, s string) bool {
	if prev == ' ' || (len(s) > 1 && s[1] == s[0]) {
		return false
	}
	if s[0] == '_' && len(s) > 1 {
		next, _ := utf8.DecodeRuneInString(s[1:])
		return !isWordRune(next)
	}
	return true
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
