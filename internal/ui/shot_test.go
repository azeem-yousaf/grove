package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/config"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/charmbracelet/x/ansi"
)

// TestReadmeShot renders the README screenshot. Run with
// GROVE_SHOT=docs/screenshot.svg go test ./internal/ui -run ReadmeShot.
//
// It draws an SVG rather than pasting terminal text into the README because
// GitHub's code font has no glyphs for ⚠ ● ■ ▾ and falls back to fonts with
// other widths, which breaks the alignment. Here every character is pinned
// to its cell and box borders are drawn as lines.
func TestReadmeShot(t *testing.T) {
	out := os.Getenv("GROVE_SHOT")
	if out == "" {
		t.Skip("set GROVE_SHOT to the output .svg path")
	}
	m := shotModel(t, 120, 22)
	if err := os.WriteFile(out, []byte(toSVG(m.render(), m.st.p, "grove · shop")), 0o644); err != nil {
		t.Fatal(err)
	}
}

// shotModel is a small, believable two-feature repository.
func shotModel(t *testing.T, w, h int) *Model {
	t.Helper()
	home, _ := os.UserHomeDir()
	src := func(name string) string { return filepath.Join(home, "src", name) }
	now := time.Now()

	repo := &git.Repo{Here: src("shop-auth"), Name: "shop"}
	m := New(context.Background(), Options{Repo: repo, Config: config.Default(), Version: "0.1.0"})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.Update(baseMsg{base: "main"})
	commit := func(hash, subject string, ago time.Duration) *git.Commit {
		return &git.Commit{Hash: hash, Subject: subject, Author: "Azeem", When: now.Add(-ago)}
	}
	snap := &git.Snapshot{
		Base: "main",
		States: []git.State{
			{
				Worktree: git.Worktree{Path: src("shop"), Branch: "main", Main: true},
				Base:     "main", Rev: "HEAD", Last: commit("8c1d2e4", "Release 2.3", 26*time.Hour),
			},
			{
				Worktree: git.Worktree{Path: src("shop-auth"), Branch: "feature/auth"},
				Base:     "main", Rev: "8c1d2e4", Ahead: 2,
				Last: commit("3fa91b0", "Retry API calls on timeout", 4*time.Minute),
				Files: []git.FileChange{
					{Path: "src/api/client.ts", Status: git.Modified, Added: 3, Deleted: 2},
					{Path: "src/auth/login.ts", Status: git.Modified, Added: 46, Deleted: 12, Uncommitted: true},
					{Path: "src/auth/session.ts", Status: git.Untracked, Added: 38, Uncommitted: true},
				},
				Added: 87, Deleted: 14,
			},
			{
				Worktree: git.Worktree{Path: src("shop-cart"), Branch: "feature/cart"},
				Base:     "main", Rev: "8c1d2e4", Ahead: 1,
				Last: commit("b72c5aa", "Cart totals", 2*time.Hour),
				Files: []git.FileChange{
					{Path: "src/api/client.ts", Status: git.Modified, Added: 4, Uncommitted: true},
					{Path: "src/cart/cart.ts", Status: git.Added, Added: 61},
					{Path: "src/cart/total.ts", Status: git.Modified, Added: 5, Deleted: 3},
				},
				Added: 70, Deleted: 3,
			},
		},
		Overlaps: map[string][]int{"src/api/client.ts": {1, 2}},
		Taken:    now,
	}
	m.Update(scanMsg{seq: m.scanSeq, snap: snap})
	press(m, "down") // src/api/client.ts in feature/auth

	st, f, _ := m.selectedFile()
	m.Update(diffMsg{key: diffKey(st, f), raw: strings.Join([]string{
		"diff --git a/src/api/client.ts b/src/api/client.ts",
		"index 1a2b3c4..5d6e7f8 100644",
		"--- a/src/api/client.ts",
		"+++ b/src/api/client.ts",
		"@@ -1,9 +1,10 @@",
		" import { retry } from './retry'",
		" import { ApiError } from './errors'",
		"-const TIMEOUT = 5000",
		"+const TIMEOUT = 10_000",
		"+const MAX_RETRIES = 3",
		" ",
		" export async function get(path: string) {",
		"-  const res = await fetch(BASE + path)",
		"+  const res = await retry(() => fetch(BASE + path))",
		"   if (!res.ok) throw new ApiError(res)",
		"   return res.json()",
		" }",
		"",
	}, "\n")})
	return m
}

const (
	cellW    = 8.4
	cellH    = 18.0
	fontSize = 14
	pad      = 18.0
	barH     = 34.0
)

type cell struct {
	r          rune
	fg, bg     string
	bold, wide bool
}

// toSVG draws ANSI-styled terminal output as an SVG window.
func toSVG(screen string, p palette, title string) string {
	hex := func(c interface{ RGBA() (r, g, b, a uint32) }) string {
		r, g, b, _ := c.RGBA()
		return fmt.Sprintf("#%02X%02X%02X", r>>8, g>>8, b>>8)
	}
	defFg, termBg := hex(p.text), "#0F1219"

	rows := parseANSI(screen, defFg)
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	w := pad*2 + float64(cols)*cellW
	h := barH + pad + float64(len(rows))*cellH + pad

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f">`+"\n", w, h, w, h)
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" rx="10" fill="%s"/>`+"\n", termBg)
	fmt.Fprintf(&b, `<rect width="100%%" height="%.0f" rx="10" fill="#161B24"/><rect y="%.0f" width="100%%" height="10" fill="#161B24"/>`+"\n", barH, barH-10)
	for i, c := range []string{"#FF5F57", "#FEBC2E", "#28C840"} {
		fmt.Fprintf(&b, `<circle cx="%.0f" cy="%.0f" r="6" fill="%s"/>`, 20+float64(i)*20, barH/2, c)
	}
	fmt.Fprintf(&b, "\n"+`<text x="%.1f" y="%.1f" fill="#8B93A7" font-family="ui-sans-serif, -apple-system, 'Segoe UI', sans-serif" font-size="13" text-anchor="middle">%s</text>`+"\n", w/2, barH/2+4.5, xmlEscape(title))
	fmt.Fprintf(&b, `<g font-family="ui-monospace, 'Cascadia Mono', 'SF Mono', Menlo, Consolas, 'DejaVu Sans Mono', monospace" font-size="%d">`+"\n", fontSize)

	for y, row := range rows {
		top := barH + pad + float64(y)*cellH
		// Backgrounds, merged into runs.
		for x := 0; x < len(row); {
			bg := row[x].bg
			end := x + 1
			for end < len(row) && row[end].bg == bg {
				end++
			}
			if bg != "" {
				fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`+"\n", pad+float64(x)*cellW, top, float64(end-x)*cellW+0.3, cellH+0.3, bg)
			}
			x = end
		}
		// Box drawing as strokes so borders join up exactly.
		for x, c := range row {
			if d := boxPath(c.r, pad+float64(x)*cellW, top); d != "" {
				fmt.Fprintf(&b, `<path d="%s" stroke="%s" stroke-width="1.2" fill="none"/>`+"\n", d, c.fg)
			}
		}
		// Text, one element per style run with every glyph pinned to its cell.
		for x := 0; x < len(row); {
			c := row[x]
			if c.r == ' ' || c.r == 0 || boxPath(c.r, 0, 0) != "" {
				x++
				continue
			}
			var xs []string
			var text strings.Builder
			end := x
			for end < len(row) {
				n := row[end]
				if n.fg != c.fg || n.bold != c.bold || n.r == 0 || boxPath(n.r, 0, 0) != "" {
					break
				}
				if n.r != ' ' {
					xs = append(xs, strconv.FormatFloat(pad+float64(end)*cellW, 'f', 1, 64))
					text.WriteString(xmlEscape(string(n.r)))
					if n.r == '⚠' {
						// Ask for the text glyph, not the colour emoji.
						xs = append(xs, xs[len(xs)-1])
						text.WriteRune('︎')
					}
				}
				end++
			}
			weight := ""
			if c.bold {
				weight = ` font-weight="700"`
			}
			fmt.Fprintf(&b, `<text x="%s" y="%.1f" fill="%s"%s>%s</text>`+"\n", strings.Join(xs, " "), top+cellH*0.73, c.fg, weight, text.String())
			x = end
		}
	}
	b.WriteString("</g>\n</svg>\n")
	return b.String()
}

// parseANSI splits styled output into rows of cells. A wide character is
// followed by a zero rune so columns stay aligned.
func parseANSI(s, defFg string) [][]cell {
	var rows [][]cell
	var row []cell
	fg, bg, bold := defFg, "", false
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			if j < len(s) && s[j] == 'm' {
				fg, bg, bold = applySGR(s[i+2:j], fg, bg, bold, defFg)
			}
			i = j + 1
			continue
		}
		if s[i] == '\n' {
			rows = append(rows, row)
			row = nil
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		wide := ansi.StringWidth(string(r)) == 2
		row = append(row, cell{r: r, fg: fg, bg: bg, bold: bold, wide: wide})
		if wide {
			row = append(row, cell{fg: fg, bg: bg})
		}
		i += size
	}
	return append(rows, row)
}

func applySGR(params, fg, bg string, bold bool, defFg string) (string, string, bool) {
	ps := strings.Split(params, ";")
	if params == "" {
		ps = []string{"0"}
	}
	for i := 0; i < len(ps); i++ {
		switch ps[i] {
		case "0", "":
			fg, bg, bold = defFg, "", false
		case "1":
			bold = true
		case "22":
			bold = false
		case "39":
			fg = defFg
		case "49":
			bg = ""
		case "38", "48":
			if i+4 < len(ps) && ps[i+1] == "2" {
				r, _ := strconv.Atoi(ps[i+2])
				g, _ := strconv.Atoi(ps[i+3])
				bl, _ := strconv.Atoi(ps[i+4])
				c := fmt.Sprintf("#%02X%02X%02X", r, g, bl)
				if ps[i] == "38" {
					fg = c
				} else {
					bg = c
				}
				i += 4
			}
		}
	}
	return fg, bg, bold
}

// boxPath returns an SVG path for a box-drawing rune in the cell at x, y.
func boxPath(r rune, x, y float64) string {
	cx, cy := x+cellW/2, y+cellH/2
	right, bottom := x+cellW, y+cellH
	rad := cellW / 2
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
	switch r {
	case '─':
		return "M" + f(x) + " " + f(cy) + "H" + f(right)
	case '│':
		return "M" + f(cx) + " " + f(y) + "V" + f(bottom)
	case '╭':
		return "M" + f(cx) + " " + f(bottom) + "V" + f(cy+rad) + "Q" + f(cx) + " " + f(cy) + " " + f(cx+rad) + " " + f(cy) + "H" + f(right)
	case '╮':
		return "M" + f(x) + " " + f(cy) + "H" + f(cx-rad) + "Q" + f(cx) + " " + f(cy) + " " + f(cx) + " " + f(cy+rad) + "V" + f(bottom)
	case '╰':
		return "M" + f(cx) + " " + f(y) + "V" + f(cy-rad) + "Q" + f(cx) + " " + f(cy) + " " + f(cx+rad) + " " + f(cy) + "H" + f(right)
	case '╯':
		return "M" + f(x) + " " + f(cy) + "H" + f(cx-rad) + "Q" + f(cx) + " " + f(cy) + " " + f(cx) + " " + f(cy-rad) + "V" + f(y)
	}
	return ""
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
