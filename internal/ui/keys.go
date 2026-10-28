package ui

import "charm.land/bubbles/v2/key"

type keyMap struct {
	Up, Down, PageUp, PageDown, Top, Bottom key.Binding
	Collapse, Expand, NextTree, PrevTree    key.Binding
	SwitchPane, Open, Reveal, Editor, Copy  key.Binding
	Remove, Git, New                        key.Binding
	Mode, Overlaps, Filter, Refresh         key.Binding
	Help, Quit, Back                        key.Binding
}

func defaultKeys() keyMap {
	b := func(help, desc string, keys ...string) key.Binding {
		return key.NewBinding(key.WithKeys(keys...), key.WithHelp(help, desc))
	}
	return keyMap{
		Up:         b("↑/k", "up", "up", "k"),
		Down:       b("↓/j", "down", "down", "j"),
		PageUp:     b("pgup", "page up", "pgup", "ctrl+u"),
		PageDown:   b("pgdn", "page down", "pgdown", "ctrl+d"),
		Top:        b("g", "top", "home", "g"),
		Bottom:     b("G", "bottom", "end", "G"),
		Collapse:   b("←/h", "collapse", "left", "h"),
		Expand:     b("→/l", "expand", "right", "l"),
		NextTree:   b("]", "next worktree", "]"),
		PrevTree:   b("[", "prev worktree", "["),
		SwitchPane: b("tab", "switch pane", "tab", "shift+tab"),
		Open:       b("enter", "open", "enter"),
		Reveal:     b("f", "show in folder", "f"),
		Editor:     b("e", "open worktree in editor", "e"),
		Copy:       b("y", "copy path", "y"),
		Remove:     b("d", "remove worktree", "d"),
		Git:        b(":", "run git here", ":"),
		New:        b("n", "new worktree", "n"),
		Mode:       b("m", "branch / uncommitted", "m"),
		Overlaps:   b("o", "overlaps only", "o"),
		Filter:     b("/", "filter", "/"),
		Refresh:    b("r", "refresh", "r"),
		Help:       b("?", "help", "?"),
		Quit:       b("q", "quit", "q", "ctrl+c"),
		Back:       b("esc", "back", "esc"),
	}
}

// helpSection is one titled block of the help overlay.
type helpSection struct {
	title string
	keys  [][2]string
}

// helpSections is the full keymap, grouped the way people look for it.
var helpSections = []helpSection{
	{"Navigate", [][2]string{
		{"↑↓ jk", "move"},
		{"← →", "fold / unfold"},
		{"[ ]", "prev / next worktree"},
		{"g G", "top / bottom"},
		{"tab", "switch pane"},
	}},
	{"Act", [][2]string{
		{"enter", "open file"},
		{"f", "show in folder"},
		{"e", "open in editor"},
		{"y", "copy path"},
		{"n", "new worktree"},
		{"d", "remove worktree"},
		{":", "run a git command"},
	}},
	{"View", [][2]string{
		{"m", "branch / uncommitted"},
		{"o", "overlaps only"},
		{"/", "filter files"},
		{"r", "refresh now"},
		{"q", "quit"},
	}},
	{"Diff pane", [][2]string{
		{"↑↓ ←→", "scroll"},
		{"←", "back to list at edge"},
		{"n N", "next / prev hunk"},
		{"[ ]", "prev / next file"},
		{"g G", "top / bottom"},
	}},
}
