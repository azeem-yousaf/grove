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
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/charmbracelet/x/ansi"
)

type lineKind uint8

const (
	lineContext lineKind = iota
	lineAdd
	lineDel
	lineHunk
	lineNote
	lineFile // a file's name, in a diff of many files
)

type diffLine struct {
	kind     lineKind
	hunk     int    // index into parsedDiff.hunks, -1 outside any
	old, new int    // line numbers; 0 when not applicable
	text     string // without the leading +/-/space
	// hiStart/hiEnd mark the changed part of a line that was edited rather
	// than wholly added or removed (byte offsets into text). hiEnd == 0 means
	// no word-level highlight.
	hiStart, hiEnd int
}

type parsedDiff struct {
	lines     []diffLine
	hunks     []git.Hunk // diffLine.hunk indexes these
	binary    bool
	truncated bool
	maxLineNo int
}

const (
	maxDiffLines     = 4000
	maxWorktreeLines = 20000 // for a whole worktree's diff (v)
)

// parseDiff turns one file's unified diff into display lines, skipping
// git's file headers and pairing edited lines for word-level highlights.
func parseDiff(raw string) parsedDiff { return parseDiffs(raw, false, maxDiffLines) }

// parseDiffs is parseDiff for a diff of many files: with files set, each
// file starts with a lineFile line naming it.
func parseDiffs(raw string, files bool, limit int) parsedDiff {
	var (
		pd       parsedDiff
		old, new int
		inHunk   bool
		hunk     = -1
	)
	pd.hunks = git.ParseHunks(raw)
	add := func(l diffLine) {
		l.hunk = hunk
		pd.lines = append(pd.lines, l)
	}
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimSuffix(l, "\r")
		if len(pd.lines) >= limit {
			pd.truncated = true
			break
		}
		switch {
		case strings.HasPrefix(l, "diff --git"):
			inHunk = false
			if files {
				// Belongs to no hunk; the count of hunks carries on.
				pd.lines = append(pd.lines, diffLine{kind: lineFile, hunk: -1, text: diffFileName(l)})
			}
		case strings.HasPrefix(l, "@@"):
			inHunk = true
			hunk++
			o, n, heading := parseHunkHeader(l)
			old, new = o, n
			// A hunk marker at the very top of the file adds nothing.
			top := len(pd.lines) == 0 || pd.lines[len(pd.lines)-1].kind == lineFile
			if top && o <= 1 && n <= 1 && heading == "" {
				continue
			}
			add(diffLine{kind: lineHunk, text: heading, old: o, new: n})
		case !inHunk:
			if strings.HasPrefix(l, "Binary files ") || strings.HasPrefix(l, "GIT binary patch") {
				pd.binary = true
				if files {
					pd.lines = append(pd.lines, diffLine{kind: lineNote, hunk: -1, text: "binary file"})
				}
			}
		case strings.HasPrefix(l, "+"):
			add(diffLine{kind: lineAdd, new: new, text: l[1:]})
			new++
		case strings.HasPrefix(l, "-"):
			add(diffLine{kind: lineDel, old: old, text: l[1:]})
			old++
		case strings.HasPrefix(l, " "):
			add(diffLine{kind: lineContext, old: old, new: new, text: l[1:]})
			old++
			new++
		case strings.HasPrefix(l, `\`):
			add(diffLine{kind: lineNote, text: strings.TrimSpace(l[1:])})
		}
		pd.maxLineNo = max(pd.maxLineNo, old, new)
	}
	if files {
		pd.binary = false // noted per file instead
	}
	// The last hunk may be cut short, so it can't be acted on.
	if pd.truncated && hunk >= 0 && hunk < len(pd.hunks) {
		pd.hunks = pd.hunks[:hunk]
	}
	if hunk+1 != len(pd.hunks) && !pd.truncated {
		// The two parsers disagree about the hunks; don't trust either.
		pd.hunks = nil
	}
	markWordChanges(pd.lines)
	return pd
}

// diffFileName is the path a "diff --git a/x b/y" line names.
func diffFileName(l string) string {
	rest := strings.TrimPrefix(l, "diff --git ")
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:]
	}
	return rest
}

// hunkAt is the hunk around line i of the diff (the first one below it when
// i is above every hunk), or false when there is none to act on.
func (pd parsedDiff) hunkAt(i int) (git.Hunk, bool) {
	if len(pd.lines) == 0 {
		return git.Hunk{}, false
	}
	i = max(0, min(i, len(pd.lines)-1))
	for j := i; j < len(pd.lines); j++ {
		if h := pd.lines[j].hunk; h >= 0 {
			if pd.lines[j].kind == lineFile {
				continue
			}
			if h < len(pd.hunks) {
				return pd.hunks[h], true
			}
			return git.Hunk{}, false
		}
		if pd.lines[j].kind == lineFile && j > i {
			break
		}
	}
	for j := i; j >= 0; j-- {
		if h := pd.lines[j].hunk; h >= 0 && h < len(pd.hunks) {
			return pd.hunks[h], true
		}
	}
	return git.Hunk{}, false
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

// clashingHunks marks every line of each hunk whose changed lines fall in
// clash, the lines predicted to conflict with another worktree. A removed
// line counts as the new-side line before it, the way the prediction places
// a pure deletion.
func clashingHunks(lines []diffLine, clash []git.LineRange) []bool {
	marked := make([]bool, len(lines))
	if len(clash) == 0 {
		return marked
	}
	in := func(n int) bool {
		for _, r := range clash {
			if n >= r.Start && n <= r.End {
				return true
			}
		}
		return false
	}
	start, next, hit := 0, 1, false
	flush := func(end int) {
		for i := start; i < end; i++ {
			marked[i] = hit
		}
	}
	for i, l := range lines {
		switch l.kind {
		case lineHunk:
			flush(i)
			start, next, hit = i, l.new, false
		case lineAdd:
			hit = hit || in(l.new)
			next = l.new + 1
		case lineContext:
			next = l.new + 1
		case lineDel:
			hit = hit || in(max(next-1, 1))
		}
	}
	flush(len(lines))
	return marked
}

// renderDiff draws a parsed diff to width columns. It returns the content
// lines and a matching gutter of line numbers, which stays put while the
// content scrolls sideways. Hunks touching clash get a bar in the gutter and
// a tinted header. In a diff of many files each lineFile switches the
// syntax highlighting to that file. The first match of search (lower case)
// on each line is highlighted.
func renderDiff(pd parsedDiff, filename string, width int, st styles, clash []git.LineRange, search string) (content, gutter []string) {
	p := st.p
	h := newHighlighter(filename, p)
	// hi is the part of a line to highlight: the search match, else the
	// word-level change.
	hi := func(l diffLine) (int, int, bool) {
		if search != "" {
			if i := strings.Index(strings.ToLower(l.text), search); i >= 0 {
				return tabShift(l.text, i), tabShift(l.text, i+len(search)), true
			}
		}
		return tabShift(l.text, l.hiStart), tabShift(l.text, l.hiEnd), false
	}
	marked := clashingHunks(pd.lines, clash)
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
	plainSep := st.faint.Render("│") + " "
	clashSep := st.conflict.Render("┃") + " "
	for i, l := range pd.lines {
		text := expandTabs(l.text)
		sep := plainSep
		if marked[i] {
			sep = clashSep
		}
		start, end, found := hi(l)
		addHi, delHi := p.addBgHi, p.delBgHi
		if found {
			addHi, delHi = p.searchBg, p.searchBg
		}
		switch l.kind {
		case lineFile:
			h = newHighlighter(l.text, p)
			bg := p.selBg
			name := st.bold.Background(bg).Render(" ▍" + ansi.Truncate(l.text, width-4, "…"))
			if found {
				name = st.bold.Background(p.searchBg).Render(" ▍" + ansi.Truncate(l.text, width-4, "…"))
			}
			gutter = append(gutter, lipgloss.NewStyle().Background(bg).Render(strings.Repeat(" ", gutterWidth)))
			content = append(content, pad(name, bg))
		case lineHunk:
			label := "⋯"
			if l.text != "" {
				label = "⋯ " + l.text
			}
			bg, hs := p.hunkBg, lipgloss.NewStyle().Foreground(p.accentAlt).Background(p.hunkBg)
			if marked[i] {
				label = "✗ conflicts " + label
				bg, hs = p.conflictBg, lipgloss.NewStyle().Foreground(p.conflict).Background(p.conflictBg).Bold(true)
			}
			gutter = append(gutter, lipgloss.NewStyle().Background(bg).Render(strings.Repeat(" ", gutterWidth)))
			content = append(content, pad(hs.Render(" "+ansi.Truncate(label, codeWidth-2, "…")), bg))
		case lineAdd:
			gutter = append(gutter, num(0, muted)+" "+num(l.new, st.add)+" "+sep)
			content = append(content, pad(st.add.Background(p.addBg).Render("+ ")+h.line(text, p.addBg, addHi, start, end), p.addBg))
		case lineDel:
			gutter = append(gutter, num(l.old, st.del)+" "+num(0, muted)+" "+sep)
			content = append(content, pad(st.del.Background(p.delBg).Render("- ")+h.line(text, p.delBg, delHi, start, end), p.delBg))
		case lineNote:
			gutter = append(gutter, strings.Repeat(" ", nw*2+2)+sep)
			content = append(content, pad(st.faint.Italic(true).Render("  "+l.text), nil))
		default:
			gutter = append(gutter, num(l.old, muted)+" "+num(l.new, muted)+" "+sep)
			if !found {
				start, end = 0, 0
			}
			content = append(content, pad("  "+h.line(text, nil, p.searchBg, start, end), nil))
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
