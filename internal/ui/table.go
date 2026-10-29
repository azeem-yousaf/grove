package ui

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// tableLen is how many of lines, from the first, make up a Markdown table:
// a header row, a rule row such as |---|:--:|, then the body rows. It is 0
// when lines don't start with one.
func tableLen(lines []string) int {
	if len(lines) < 2 || !isTableRow(lines[0]) || tableAligns(lines[1]) == nil {
		return 0
	}
	n := 2
	for n < len(lines) && isTableRow(lines[n]) {
		n++
	}
	return n
}

func isTableRow(l string) bool {
	return strings.HasPrefix(strings.TrimSpace(l), "|")
}

// cellLink is a Markdown link inside a table cell, shown as its text.
var cellLink = regexp.MustCompile(`\[([^\]]+)\]\([^)\s]+\)`)

// tableCells splits a row into its cells, without Markdown emphasis, code
// marks or link targets, which would only take up room in a terminal.
func tableCells(l string) []string {
	l = strings.TrimSpace(l)
	l = strings.TrimPrefix(l, "|")
	if !strings.HasSuffix(l, `\|`) {
		l = strings.TrimSuffix(l, "|")
	}
	l = strings.ReplaceAll(l, `\|`, "\x00")
	clean := strings.NewReplacer("\x00", "|", "**", "", "`", "")
	var cells []string
	for _, c := range strings.Split(l, "|") {
		c = cellLink.ReplaceAllString(clean.Replace(c), "$1")
		cells = append(cells, strings.TrimSpace(c))
	}
	return cells
}

// tableAligns reads a rule row as one alignment per column ('l', 'c' or
// 'r'), or returns nil if l isn't a rule row.
func tableAligns(l string) []byte {
	if !isTableRow(l) {
		return nil
	}
	var aligns []byte
	for _, c := range tableCells(l) {
		dashes := strings.Trim(c, ":")
		if dashes == "" || strings.Trim(dashes, "-") != "" {
			return nil
		}
		switch {
		case strings.HasPrefix(c, ":") && strings.HasSuffix(c, ":"):
			aligns = append(aligns, 'c')
		case strings.HasSuffix(c, ":"):
			aligns = append(aligns, 'r')
		default:
			aligns = append(aligns, 'l')
		}
	}
	return aligns
}

// renderTable draws a Markdown table (as measured by tableLen) in columns
// that fit width, wrapping cells between words. When that can't fit
// without splitting a word, each row becomes a short list of
// "header: value" lines.
func renderTable(lines []string, width int, st styles, style lipgloss.Style) []string {
	header, aligns := tableCells(lines[0]), tableAligns(lines[1])
	var body [][]string
	for _, l := range lines[2:] {
		body = append(body, tableCells(l))
	}
	cols := len(header)
	for _, r := range body {
		cols = max(cols, len(r))
	}
	cell := func(r []string, j int) string {
		if j < len(r) {
			return r[j]
		}
		return ""
	}

	// Columns start as wide as their widest cell and can narrow to their
	// longest word.
	widths, words := make([]int, cols), make([]int, cols)
	for _, r := range append([][]string{header}, body...) {
		for j := range cols {
			widths[j] = max(widths[j], ansi.StringWidth(cell(r, j)), 1)
			for _, w := range strings.FieldsFunc(cell(r, j), func(c rune) bool { return c == ' ' || c == '/' || c == '-' }) {
				words[j] = max(words[j], ansi.StringWidth(w)+1) // +1 for the / or - kept at a line end
			}
		}
	}
	least := 0
	for j := range cols {
		words[j] = min(words[j], widths[j])
		least += words[j]
	}

	const sep = " │ "
	room := width - len([]rune(sep))*(cols-1)
	if room < least {
		var out []string
		for i, r := range body {
			if i > 0 {
				out = append(out, "")
			}
			for j := range cols {
				line := style.Bold(true).Render(cell(header, j)+": ") + style.Render(cell(r, j))
				for k, l := range strings.Split(ansi.Wrap(line, max(width-2, 1), "/"), "\n") {
					if k > 0 {
						l = "  " + l // under the value, clear of the header
					}
					out = append(out, l)
				}
			}
		}
		return out
	}

	// Narrow the widest column that is still wider than its longest word
	// until the table fits, so no word is split.
	for total := sum(widths); total > room; total-- {
		w := -1
		for j := range widths {
			if widths[j] > words[j] && (w < 0 || widths[j] > widths[w]) {
				w = j
			}
		}
		widths[w]--
	}

	wrap := func(r []string) ([][]string, int) {
		wrapped := make([][]string, cols)
		height := 1
		for j := range cols {
			wrapped[j] = strings.Split(ansi.Wrap(cell(r, j), widths[j], "/"), "\n")
			height = max(height, len(wrapped[j]))
		}
		return wrapped, height
	}
	var out []string
	row := func(r []string, s lipgloss.Style) {
		wrapped, height := wrap(r)
		for k := range height {
			var b strings.Builder
			for j := range cols {
				if j > 0 {
					b.WriteString(st.faint.Render(sep))
				}
				text := ""
				if k < len(wrapped[j]) {
					text = wrapped[j][k]
				}
				align := byte('l')
				if j < len(aligns) {
					align = aligns[j]
				}
				b.WriteString(s.Render(padCell(text, widths[j], align)))
			}
			out = append(out, b.String())
		}
	}
	rule := make([]string, cols)
	for j, w := range widths {
		rule[j] = strings.Repeat("─", w)
	}
	line := st.faint.Render(strings.Join(rule, "─┼─"))
	// Once a cell takes more than one line, a rule between rows shows
	// where each row ends.
	ruled := false
	for _, r := range body {
		if _, h := wrap(r); h > 1 {
			ruled = true
		}
	}
	row(header, style.Bold(true))
	out = append(out, line)
	for i, r := range body {
		if ruled && i > 0 {
			out = append(out, line)
		}
		row(r, style)
	}
	return out
}

func padCell(s string, w int, align byte) string {
	gap := w - ansi.StringWidth(s)
	if gap <= 0 {
		return s
	}
	switch align {
	case 'r':
		return strings.Repeat(" ", gap) + s
	case 'c':
		return strings.Repeat(" ", gap/2) + s + strings.Repeat(" ", gap-gap/2)
	}
	return s + strings.Repeat(" ", gap)
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}
