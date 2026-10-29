package ui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// palette is grove's colour system. Every colour in the UI comes from here so
// light and dark terminals each get a tuned set instead of a compromise.
type palette struct {
	dark bool

	text, muted, faint color.Color
	border, focus      color.Color
	selBg, selBgDim    color.Color
	accent, accentAlt  color.Color
	add, del, warn     color.Color
	info, renamed      color.Color
	addBg, delBg       color.Color
	addBgHi, delBgHi   color.Color
	hunkBg, panelBg    color.Color
	conflict           color.Color // files and hunks that will conflict when merged
	conflictBg         color.Color
	chroma             string
}

func newPalette(dark bool) palette {
	c := lipgloss.Color
	if dark {
		return palette{
			dark:       true,
			text:       c("#E6E9EF"),
			muted:      c("#8B93A7"),
			faint:      c("#4B5263"),
			border:     c("#2C3240"),
			focus:      c("#5EEAD4"),
			selBg:      c("#1E2A33"),
			selBgDim:   c("#1A1F29"),
			accent:     c("#5EEAD4"),
			accentAlt:  c("#A78BFA"),
			add:        c("#4ADE80"),
			del:        c("#F87171"),
			warn:       c("#FBBF24"),
			info:       c("#60A5FA"),
			renamed:    c("#C084FC"),
			addBg:      c("#11261B"),
			delBg:      c("#2A1517"),
			addBgHi:    c("#1B3D2A"),
			delBgHi:    c("#4A1F23"),
			hunkBg:     c("#161C27"),
			panelBg:    c("#141821"),
			conflict:   c("#FB7185"),
			conflictBg: c("#33141D"),
			chroma:     "catppuccin-mocha",
		}
	}
	return palette{
		text:       c("#1F2430"),
		muted:      c("#5F6779"),
		faint:      c("#A7AEBD"),
		border:     c("#D5DAE3"),
		focus:      c("#0D9488"),
		selBg:      c("#E3F2EF"),
		selBgDim:   c("#EEF1F5"),
		accent:     c("#0F766E"),
		accentAlt:  c("#7C3AED"),
		add:        c("#15803D"),
		del:        c("#B91C1C"),
		warn:       c("#B45309"),
		info:       c("#1D4ED8"),
		renamed:    c("#7E22CE"),
		addBg:      c("#E7F7EC"),
		delBg:      c("#FCEBEC"),
		addBgHi:    c("#C6EBD2"),
		delBgHi:    c("#F7C9CD"),
		hunkBg:     c("#EEF2F8"),
		panelBg:    c("#F6F8FB"),
		conflict:   c("#BE123C"),
		conflictBg: c("#FDE2E7"),
		chroma:     "github",
	}
}

// styles are the reusable lipgloss styles derived from a palette.
type styles struct {
	p palette

	text, muted, faint, bold lipgloss.Style
	accent, add, del, warn   lipgloss.Style
	info, renamed, conflict  lipgloss.Style

	pane, paneFocused      lipgloss.Style
	paneTitle              lipgloss.Style
	pill, pillAccent       lipgloss.Style
	pillWarn, pillConflict lipgloss.Style
	key, keyDesc           lipgloss.Style
	modal                  lipgloss.Style
}

func newStyles(dark bool) styles {
	p := newPalette(dark)
	s := lipgloss.NewStyle
	base := s().Foreground(p.text)
	return styles{
		p:        p,
		text:     base,
		muted:    s().Foreground(p.muted),
		faint:    s().Foreground(p.faint),
		bold:     base.Bold(true),
		accent:   s().Foreground(p.accent),
		add:      s().Foreground(p.add),
		del:      s().Foreground(p.del),
		warn:     s().Foreground(p.warn),
		info:     s().Foreground(p.info),
		renamed:  s().Foreground(p.renamed),
		conflict: s().Foreground(p.conflict).Bold(true),

		pane: s().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.border),
		paneFocused: s().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.focus),
		paneTitle: s().Foreground(p.text).Bold(true),

		pill:         s().Foreground(p.muted).Background(p.selBgDim).Padding(0, 1),
		pillAccent:   s().Foreground(p.panelBg).Background(p.accent).Bold(true).Padding(0, 1),
		pillWarn:     s().Foreground(p.panelBg).Background(p.warn).Bold(true).Padding(0, 1),
		pillConflict: s().Foreground(p.panelBg).Background(p.conflict).Bold(true).Padding(0, 1),

		key:     s().Foreground(p.accent).Bold(true),
		keyDesc: s().Foreground(p.muted),

		modal: s().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.accent).
			Padding(1, 3),
	}
}
