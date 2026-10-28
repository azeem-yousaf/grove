package ui

import (
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

// tableCells splits a row into its cells, without Markdown emphasis or
// code marks, which would only take up room in a terminal.
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
		cells = append(cells, strings.TrimSpace(clean.Replace(c)))
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
// that fit width, wrapping cells that don't. When even that won't fit,
// each row becomes a short list of "header: value" lines.
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

	const sep = " │ "
	room := width - len([]rune(sep))*(cols-1)
	if room < cols*4 {
		var out []string
		for i, r := range body {
			if i > 0 {
				out = append(out, "")
			}
			for j := range cols {
				line := style.Bold(true).Render(cell(header, j)+": ") + style.Render(cell(r, j))
				out = append(out, strings.Split(ansi.Wrap(line, width, ""), "\n")...)
			}
		}
		return out
	}

	widths := make([]int, cols)
	for _, r := range append([][]string{header}, body...) {
		for j := range cols {
			widths[j] = max(widths[j], ansi.StringWidth(cell(r, j)), 1)
		}
	}
	// Narrow the widest column until the table fits.
	for total := sum(widths); total > room; total-- {
		w := 0
		for j := range widths {
			if widths[j] > widths[w] {
				w = j
			}
		}
		widths[w]--
	}

	var out []string
	row := func(r []string, s lipgloss.Style) {
		wrapped := make([][]string, cols)
		height := 1
		for j := range cols {
			wrapped[j] = strings.Split(ansi.Wrap(cell(r, j), widths[j], ""), "\n")
			height = max(height, len(wrapped[j]))
		}
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
	row(header, style.Bold(true))
	rule := make([]string, cols)
	for j, w := range widths {
		rule[j] = strings.Repeat("─", w)
	}
	out = append(out, st.faint.Render(strings.Join(rule, "─┼─")))
	for _, r := range body {
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
