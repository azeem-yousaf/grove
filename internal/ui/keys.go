package ui

import "charm.land/bubbles/v2/key"

type keyMap struct {
	Up, Down, PageUp, PageDown, Top, Bottom key.Binding
	Collapse, Expand, NextTree, PrevTree    key.Binding
	SwitchPane, Open, Reveal, Editor, Copy  key.Binding
	Remove, Git, New, Agent, Undo           key.Binding
	Mode, Overlaps, Filter, Refresh         key.Binding
	Overview, Merge, Planner, Group, Ship   key.Binding
	Review, Usage                           key.Binding
	Help, Actions, Quit, Back               key.Binding
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
		Agent:      b("a", "worktree agent", "a"),
		Undo:       b("u", "undo agent changes", "u"),
		Mode:       b("m", "branch / uncommitted", "m"),
		Overlaps:   b("o", "overlaps / conflicts only", "o"),
		Overview:   b("w", "overview of every worktree", "w"),
		Usage:      b("T", "token usage", "T"),
		Merge:      b("c", "merge branches into this worktree", "c"),
		Planner:    b("P", "planner", "P"),
		Group:      b("t", "group or plan chat", "t"),
		Ship:       b("s", "commit, push, pull request", "s"),
		Review:     b("v", "every change in the worktree", "v"),
		Filter:     b("/", "filter", "/"),
		Refresh:    b("r", "refresh", "r"),
		Help:       b("?", "help", "?"),
		Actions:    b("ctrl+k", "actions", "ctrl+k"),
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
		{"s", "commit, push, PR, update"},
		{"ctrl+k", "search all actions"},
	}},
	{"View", [][2]string{
		{"m", "branch / uncommitted"},
		{"o", "overlaps / conflicts"},
		{"w", "overview of worktrees"},
		{"T", "token usage"},
		{"c", "merge branches here"},
		{"/", "filter files"},
		{"r", "refresh now"},
		{"q", "quit"},
	}},
	{"Agent", [][2]string{
		{"a", "open worktree agent"},
		{"P", "planner: plan tasks (on a group: its planner)"},
		{"t", "group or plan chat"},
		{"enter", "send / allow / run"},
		{"esc", "deny or skip a card"},
		{"ctrl+x", "stop the agent"},
		{"ctrl+z u", "undo its changes"},
		{"ctrl+a", "claude / codex"},
		{"/model", "choose its model"},
	}},
	{"Diff pane", [][2]string{
		{"↑↓ ←→", "scroll"},
		{"←", "back to list at edge"},
		{"n N", "next / prev hunk"},
		{"[ ]", "prev / next file"},
		{"v", "whole worktree, last turn"},
		{"/", "search; n N next"},
		{"x", "put a change back"},
		{">", "ask the agent about it"},
		{"g G", "top / bottom"},
	}},
}
