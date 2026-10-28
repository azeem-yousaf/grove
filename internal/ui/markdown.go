package ui

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var (
	// listItem is a Markdown list item: its indent, marker and text.
	listItem = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+(.*)$`)
	heading  = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
	hrule    = regexp.MustCompile(`^(-{3,}|\*{3,}|_{3,})$`)
)

// renderProse wraps one line of Markdown prose to width. Headings are
// bold, list items get a bullet and a hanging indent so wrapped lines line
// up with their text, and inline code and bold lose their marks.
func renderProse(l string, width int, st styles, style lipgloss.Style) []string {
	trimmed := strings.TrimSpace(l)
	if hrule.MatchString(trimmed) {
		return []string{st.faint.Render(strings.Repeat("─", max(width, 1)))}
	}
	first, text := "", strings.TrimRight(l, " ")
	if m := heading.FindStringSubmatch(trimmed); m != nil {
		style, text = style.Bold(true), m[1]
	} else if m := listItem.FindStringSubmatch(text); m != nil {
		marker := m[2]
		if strings.ContainsAny(marker, "-*+") {
			marker = "•"
		}
		first, text = expandTabs(m[1])+marker+" ", m[3]
	}
	hang := strings.Repeat(" ", ansi.StringWidth(first))
	wrapped := lipgloss.NewStyle().Width(max(width-len(hang), 10)).Render(inline(text, st, style))
	lines := strings.Split(wrapped, "\n")
	for i := range lines {
		p := hang
		if i == 0 {
			p = first
		}
		lines[i] = st.faint.Render(p) + lines[i]
	}
	return lines
}

// inline styles Markdown code spans and bold text, dropping their marks.
// A mark with no partner is shown as it is.
func inline(s string, st styles, base lipgloss.Style) string {
	var b strings.Builder
	bold := false
	for s != "" {
		if strings.HasPrefix(s, "`") {
			if end := strings.IndexByte(s[1:], '`'); end > 0 {
				code := st.info
				if bold {
					code = code.Bold(true)
				}
				b.WriteString(code.Render(s[1 : end+1]))
				s = s[end+2:]
				continue
			}
		} else if strings.HasPrefix(s, "**") && (bold || strings.Contains(s[2:], "**")) {
			bold = !bold
			s = s[2:]
			continue
		}
		// Plain text up to the next possible mark.
		run := s
		if n := strings.IndexAny(s[1:], "`*"); n >= 0 {
			run = s[:n+1]
		}
		style := base
		if bold {
			style = base.Bold(true)
		}
		b.WriteString(style.Render(run))
		s = s[len(run):]
	}
	return b.String()
}
