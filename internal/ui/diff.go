package ui

import (
	"fmt"
	"image/color"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	chromastyles "github.com/alecthomas/chroma/v2/styles"
	"github.com/charmbracelet/x/ansi"
)

type lineKind uint8

const (
	lineContext lineKind = iota
	lineAdd
	lineDel
	lineHunk
	lineNote
)

type diffLine struct {
	kind     lineKind
	old, new int    // line numbers; 0 when not applicable
	text     string // without the leading +/-/space
	// hiStart/hiEnd mark the changed part of a line that was edited rather
	// than wholly added or removed (byte offsets into text). hiEnd == 0 means
	// no word-level highlight.
	hiStart, hiEnd int
}

type parsedDiff struct {
	lines     []diffLine
	binary    bool
	truncated bool
	maxLineNo int
}

const maxDiffLines = 4000

// parseDiff turns unified diff text into display lines, skipping git's file
// headers and pairing edited lines for word-level highlights.
func parseDiff(raw string) parsedDiff {
	var (
		pd       parsedDiff
		old, new int
		inHunk   bool
	)
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimSuffix(l, "\r")
		if len(pd.lines) >= maxDiffLines {
			pd.truncated = true
			break
		}
		switch {
		case strings.HasPrefix(l, "@@"):
			inHunk = true
			o, n, heading := parseHunkHeader(l)
			old, new = o, n
			// A hunk marker at the very top of the file adds nothing.
			if len(pd.lines) == 0 && o <= 1 && n <= 1 && heading == "" {
				continue
			}
			pd.lines = append(pd.lines, diffLine{kind: lineHunk, text: heading, old: o, new: n})
		case !inHunk:
			if strings.HasPrefix(l, "Binary files ") || strings.HasPrefix(l, "GIT binary patch") {
				pd.binary = true
			}
		case strings.HasPrefix(l, "+"):
			pd.lines = append(pd.lines, diffLine{kind: lineAdd, new: new, text: l[1:]})
			new++
		case strings.HasPrefix(l, "-"):
			pd.lines = append(pd.lines, diffLine{kind: lineDel, old: old, text: l[1:]})
			old++
		case strings.HasPrefix(l, " "):
			pd.lines = append(pd.lines, diffLine{kind: lineContext, old: old, new: new, text: l[1:]})
			old++
			new++
		case strings.HasPrefix(l, `\`):
			pd.lines = append(pd.lines, diffLine{kind: lineNote, text: strings.TrimSpace(l[1:])})
		case strings.HasPrefix(l, "diff --git"):
			inHunk = false
		}
		pd.maxLineNo = max(pd.maxLineNo, old, new)
	}
	markWordChanges(pd.lines)
	return pd
}

// parseHunkHeader reads "@@ -12,7 +12,9 @@ func heading".
func parseHunkHeader(l string) (old, new int, heading string) {
	parts := strings.SplitN(l, "@@", 3)
	if len(parts) < 3 {
		return 1, 1, ""
	}
	for _, f := range strings.Fields(parts[1]) {
		n, _, _ := strings.Cut(f[1:], ",")
		v, _ := strconv.Atoi(n)
		switch f[0] {
		case '-':
			old = v
		case '+':
			new = v
		}
	}
	return max(old, 1), max(new, 1), strings.TrimSpace(parts[2])
}

// markWordChanges finds runs of removed lines followed by the same number of
// added lines and marks the part of each pair that actually changed, so an
// edited line highlights the edit rather than the whole line.
func markWordChanges(lines []diffLine) {
	for i := 0; i < len(lines); {
		if lines[i].kind != lineDel {
			i++
			continue
		}
		d := i
		for d < len(lines) && lines[d].kind == lineDel {
			d++
		}
		a := d
		for a < len(lines) && lines[a].kind == lineAdd {
			a++
		}
		if dels, adds := d-i, a-d; dels == adds && dels <= 8 {
			for k := 0; k < dels; k++ {
				pairHighlight(&lines[i+k], &lines[d+k])
			}
		}
		i = a
	}
}

func pairHighlight(del, add *diffLine) {
	a, b := del.text, add.text
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	// Only worth it when the lines share a meaningful amount of text.
	if p+s < 4 {
		return
	}
	if len(a)-s > p {
		del.hiStart, del.hiEnd = wordBounds(a, p, len(a)-s)
	}
	if len(b)-s > p {
		add.hiStart, add.hiEnd = wordBounds(b, p, len(b)-s)
	}
}

// wordBounds widens [start, end) to whole words, so "5000" -> "10000"
// highlights both numbers rather than just the digits that differ.
func wordBounds(s string, start, end int) (int, int) {
	for start > 0 && isWordByte(s[start-1]) {
		start--
	}
	for end < len(s) && isWordByte(s[end]) {
		end++
	}
	return start, end
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// highlighter renders source lines with Chroma syntax colours on top of the
// diff backgrounds.
type highlighter struct {
	lexer chroma.Lexer
	style *chroma.Style
	cache map[styleKey]lipgloss.Style
	fg    color.Color
}

type styleKey struct {
	tt chroma.TokenType
	bg color.Color
}

func newHighlighter(filename string, p palette) *highlighter {
	lexer := lexers.Match(filename)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	style := chromastyles.Get(p.chroma)
	if style == nil {
		style = chromastyles.Fallback
	}
	return &highlighter{
		lexer: chroma.Coalesce(lexer),
		style: style,
		cache: map[styleKey]lipgloss.Style{},
		fg:    p.text,
	}
}

func (h *highlighter) tokenStyle(tt chroma.TokenType, bg color.Color) lipgloss.Style {
	k := styleKey{tt, bg}
	if s, ok := h.cache[k]; ok {
		return s
	}
	e := h.style.Get(tt)
	s := lipgloss.NewStyle().Foreground(h.fg)
	if e.Colour.IsSet() {
		s = s.Foreground(lipgloss.Color(e.Colour.String()))
	}
	if e.Bold == chroma.Yes {
		s = s.Bold(true)
	}
	if e.Italic == chroma.Yes {
		s = s.Italic(true)
	}
	if bg != nil {
		s = s.Background(bg)
	}
	h.cache[k] = s
	return s
}

// line renders text with syntax colours. Bytes in [hiStart, hiEnd) get hiBg
// instead of bg.
func (h *highlighter) line(text string, bg, hiBg color.Color, hiStart, hiEnd int) string {
	it, err := h.lexer.Tokenise(nil, text)
	if err != nil {
		return lipgloss.NewStyle().Foreground(h.fg).Background(bg).Render(text)
	}
	var (
		b   strings.Builder
		pos int
	)
	for _, tok := range it.Tokens() {
		val := strings.ReplaceAll(tok.Value, "\n", "")
		for val != "" {
			bgNow, n := bg, len(val)
			switch {
			case hiEnd > 0 && pos < hiStart:
				n = min(n, hiStart-pos)
			case hiEnd > 0 && pos < hiEnd:
				bgNow, n = hiBg, min(n, hiEnd-pos)
			}
			b.WriteString(h.tokenStyle(tok.Type, bgNow).Render(val[:n]))
			val, pos = val[n:], pos+n
		}
	}
	return b.String()
}

// renderDiff draws a parsed diff to width columns. It returns the content
// lines and a matching gutter of line numbers, which stays put while the
// content scrolls sideways.
func renderDiff(pd parsedDiff, filename string, width int, st styles) (content, gutter []string) {
	p := st.p
	h := newHighlighter(filename, p)
	nw := max(len(strconv.Itoa(pd.maxLineNo)), 3)
	gutterWidth := nw*2 + 4
	codeWidth := max(width-gutterWidth, 10)

	num := func(n int, s lipgloss.Style) string {
		if n == 0 {
			return strings.Repeat(" ", nw)
		}
		return s.Render(fmt.Sprintf("%*d", nw, n))
	}
	pad := func(s string, bg color.Color) string {
		if w := ansi.StringWidth(s); w < codeWidth {
			fill := lipgloss.NewStyle()
			if bg != nil {
				fill = fill.Background(bg)
			}
			s += fill.Render(strings.Repeat(" ", codeWidth-w))
		}
		return s
	}

	muted := st.faint
	sep := st.faint.Render("│") + " "
	for _, l := range pd.lines {
		text := expandTabs(l.text)
		switch l.kind {
		case lineHunk:
			label := "⋯"
			if l.text != "" {
				label = "⋯ " + l.text
			}
			hs := lipgloss.NewStyle().Foreground(p.accentAlt).Background(p.hunkBg)
			gutter = append(gutter, lipgloss.NewStyle().Background(p.hunkBg).Render(strings.Repeat(" ", gutterWidth)))
			content = append(content, pad(hs.Render(" "+ansi.Truncate(label, codeWidth-2, "…")), p.hunkBg))
		case lineAdd:
			gutter = append(gutter, num(0, muted)+" "+num(l.new, st.add)+" "+sep)
			content = append(content, pad(st.add.Background(p.addBg).Render("+ ")+h.line(text, p.addBg, p.addBgHi, tabShift(l.text, l.hiStart), tabShift(l.text, l.hiEnd)), p.addBg))
		case lineDel:
			gutter = append(gutter, num(l.old, st.del)+" "+num(0, muted)+" "+sep)
			content = append(content, pad(st.del.Background(p.delBg).Render("- ")+h.line(text, p.delBg, p.delBgHi, tabShift(l.text, l.hiStart), tabShift(l.text, l.hiEnd)), p.delBg))
		case lineNote:
			gutter = append(gutter, strings.Repeat(" ", nw*2+2)+sep)
			content = append(content, pad(st.faint.Italic(true).Render("  "+l.text), nil))
		default:
			gutter = append(gutter, num(l.old, muted)+" "+num(l.new, muted)+" "+sep)
			content = append(content, pad("  "+h.line(text, nil, nil, 0, 0), nil))
		}
	}
	return content, gutter
}

const tabWidth = 4

func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := tabWidth - col%tabWidth
			b.WriteString(strings.Repeat(" ", n))
			col += n
			continue
		}
		b.WriteRune(r)
		col++
	}
	return b.String()
}

// tabShift converts a byte offset in the original text to the matching
// offset after tab expansion.
func tabShift(s string, off int) int {
	if off == 0 || !strings.Contains(s[:min(off, len(s))], "\t") {
		return off
	}
	return len(expandTabs(s[:off]))
}
