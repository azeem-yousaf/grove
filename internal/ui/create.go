package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/charmbracelet/x/ansi"
)

// Fields of the new-worktree dialog, in tab order.
const (
	fieldBranch = iota
	fieldFrom
	fieldFolder
	fieldCopy
	fieldSetup
	fieldEditor
)

// creation is the new-worktree dialog.
type creation struct {
	inputs       [3]textinput.Model // branch, from, folder
	focus        int
	folderEdited bool // the user typed their own folder; stop suggesting
	autoFolder   string

	branches *git.Branches // nil while loading
	loadErr  error

	mainPath  string   // copy files come from here
	copyFiles []string // relative paths matched by [new] copy
	copy      bool
	setup     bool
	editor    bool

	running bool
	err     string
}

type (
	branchesMsg struct {
		b   git.Branches
		err error
	}
	createdMsg struct {
		path, branch string
		copied       int
		copyErr      error
		err          error
		c            *creation
	}
)

// askCreate opens the dialog.
func (m *Model) askCreate() tea.Cmd {
	if m.snap == nil {
		return nil
	}
	c := &creation{editor: m.opener.Editor != "", setup: m.cfg.New.Setup != ""}
	labels := [3]string{"feature/name", "", ""}
	for i := range c.inputs {
		ti := textinput.New()
		ti.Prompt = ""
		ti.Placeholder = labels[i]
		ti.SetStyles(textinput.DefaultStyles(m.dark))
		c.inputs[i] = ti
	}
	c.inputs[fieldFrom].SetValue(m.base)
	if m.base == "" {
		c.inputs[fieldFrom].Placeholder = "HEAD"
	}
	for _, st := range m.snap.States {
		if st.Main && !st.Bare {
			c.mainPath = st.Path
		}
	}
	if c.mainPath != "" {
		c.copyFiles = git.MatchCopy(c.mainPath, m.cfg.New.Copy)
		c.copy = len(c.copyFiles) > 0
	}
	m.creating = c
	repo := m.repo
	load := func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		defer cancel()
		b, err := git.ListBranches(ctx, repo)
		return branchesMsg{b, err}
	}
	return tea.Batch(load, c.inputs[fieldBranch].Focus())
}

// fields lists the dialog rows that can take focus right now.
func (m *Model) fields() []int {
	c := m.creating
	out := []int{fieldBranch}
	if _, kind, _ := m.resolveBranch(); kind == git.BranchNew {
		out = append(out, fieldFrom)
	}
	out = append(out, fieldFolder)
	if len(c.copyFiles) > 0 {
		out = append(out, fieldCopy)
	}
	if m.cfg.New.Setup != "" {
		out = append(out, fieldSetup)
	}
	if m.opener.Editor != "" {
		out = append(out, fieldEditor)
	}
	return out
}

func (m *Model) resolveBranch() (string, git.BranchKind, string) {
	c := m.creating
	name := strings.TrimSpace(c.inputs[fieldBranch].Value())
	if c.branches == nil {
		return name, git.BranchNew, ""
	}
	return c.branches.Resolve(name)
}

// branchStatus describes the typed branch, and whether it can be used.
func (m *Model) branchStatus() (string, bool) {
	c := m.creating
	name := strings.TrimSpace(c.inputs[fieldBranch].Value())
	if name == "" {
		return "type a branch name", false
	}
	if c.branches == nil && c.loadErr == nil {
		return "checking…", false
	}
	local, kind, remote := m.resolveBranch()
	for _, st := range m.snap.States {
		if st.Branch == local {
			return "already checked out in " + st.Name() + " (" + shortPath(st.Path, 30) + ")", false
		}
	}
	switch kind {
	case git.BranchLocal:
		return "existing branch", true
	case git.BranchRemote:
		return "new local branch tracking " + remote, true
	}
	if err := git.ValidBranchName(local); err != nil {
		return err.Error(), false
	}
	return "new branch", true
}

func (m *Model) folderStatus() (string, bool) {
	p := strings.TrimSpace(m.creating.inputs[fieldFolder].Value())
	if p == "" {
		return "choose a folder", false
	}
	if _, err := os.Stat(p); err == nil {
		return "already exists", false
	}
	return "will be created", true
}

// syncFolder keeps suggesting a folder named after the branch until the
// user types their own.
func (m *Model) syncFolder() {
	c := m.creating
	if c.folderEdited || c.mainPath == "" && m.repo.CommonDir == "" {
		return
	}
	local, _, _ := m.resolveBranch()
	base := c.mainPath
	if base == "" {
		base = m.repo.CommonDir
	}
	c.autoFolder = git.DefaultWorktreePath(base, m.repo.Name, local)
	c.inputs[fieldFolder].SetValue(c.autoFolder)
}

func (m *Model) onCreateKey(msg tea.KeyPressMsg) tea.Cmd {
	c := m.creating
	if c.running {
		if msg.String() == "ctrl+c" {
			return tea.Quit
		}
		return nil
	}
	fields := m.fields()
	pos := 0
	for i, f := range fields {
		if f == c.focus {
			pos = i
		}
	}
	moveTo := func(i int) tea.Cmd {
		i = (i + len(fields)) % len(fields)
		for k := range c.inputs {
			c.inputs[k].Blur()
		}
		c.focus = fields[i]
		if c.focus <= fieldFolder {
			c.inputs[c.focus].CursorEnd()
			return c.inputs[c.focus].Focus()
		}
		return nil
	}

	switch msg.String() {
	case "esc":
		m.creating = nil
		return nil
	case "ctrl+c":
		return tea.Quit
	case "tab", "down":
		return moveTo(pos + 1)
	case "shift+tab", "up":
		return moveTo(pos - 1)
	case "enter":
		return m.create()
	case "space":
		switch c.focus {
		case fieldCopy:
			c.copy = !c.copy
			return nil
		case fieldSetup:
			c.setup = !c.setup
			return nil
		case fieldEditor:
			c.editor = !c.editor
			return nil
		}
	}
	if c.focus > fieldFolder {
		return nil
	}
	var cmd tea.Cmd
	c.inputs[c.focus], cmd = c.inputs[c.focus].Update(msg)
	c.err = ""
	switch c.focus {
	case fieldBranch:
		m.syncFolder()
	case fieldFolder:
		c.folderEdited = c.inputs[fieldFolder].Value() != c.autoFolder
	}
	return cmd
}

func (m *Model) create() tea.Cmd {
	c := m.creating
	if msg, ok := m.branchStatus(); !ok {
		c.err = "Branch: " + msg
		return nil
	}
	if msg, ok := m.folderStatus(); !ok {
		c.err = "Folder: " + msg
		return nil
	}
	c.running, c.err = true, ""
	repo, branches := m.repo, git.Branches{}
	if c.branches != nil {
		branches = *c.branches
	}
	opt := git.AddOptions{
		Path:   filepath.Clean(strings.TrimSpace(c.inputs[fieldFolder].Value())),
		Branch: strings.TrimSpace(c.inputs[fieldBranch].Value()),
		From:   strings.TrimSpace(c.inputs[fieldFrom].Value()),
	}
	if abs, err := filepath.Abs(opt.Path); err == nil {
		opt.Path = abs
	}
	run := func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Minute)
		defer cancel()
		branch, err := git.AddWorktree(ctx, repo, branches, opt)
		msg := createdMsg{path: opt.Path, branch: branch, err: err, c: c}
		if err == nil && c.copy {
			msg.copied, msg.copyErr = git.CopyInto(c.mainPath, opt.Path, c.copyFiles)
		}
		return msg
	}
	return tea.Batch(run, m.startAnimating())
}

func (m *Model) onCreated(msg createdMsg) tea.Cmd {
	if msg.err != nil {
		if m.creating == msg.c {
			msg.c.running = false
			msg.c.err = msg.err.Error()
		}
		return nil
	}
	m.creating = nil
	m.selectPath = msg.path

	text := fmt.Sprintf("Created %s in %s", msg.branch, shortPath(msg.path, 40))
	kind := toastOK
	switch {
	case msg.copyErr != nil:
		text, kind = text+"; copying files failed: "+msg.copyErr.Error(), toastErr
	case msg.copied > 0:
		text += " · copied " + plural(msg.copied, "file")
	}
	cmds := []tea.Cmd{m.notify(text, kind), m.startScan()}
	if msg.c.editor {
		opener, path := m.opener, msg.path
		cmds = append(cmds, func() tea.Msg {
			_, err := opener.Folder(path)
			if err != nil {
				return actionMsg{err: err}
			}
			return nil
		})
	}
	if msg.c.setup && m.cfg.New.Setup != "" {
		prog, args := shellCommand(m.cfg.New.Setup)
		cmds = append(cmds, m.runInTerminal(msg.branch, msg.path, prog, args, m.cfg.New.Setup))
	}
	return tea.Batch(cmds...)
}

// createModal draws the dialog.
func (m *Model) createModal() string {
	st := m.st
	c := m.creating
	w := min(72, m.width-10)
	labelW := 10
	inputW := max(w-labelW-2, 10)

	label := func(f int, text string) string {
		s := st.muted
		if c.focus == f {
			s = st.accent.Bold(true)
		}
		return s.Render(fmt.Sprintf("%-*s", labelW, text))
	}
	status := func(text string, ok bool) string {
		indent := strings.Repeat(" ", labelW)
		if ok {
			return indent + st.add.Render("✓ ") + st.faint.Render(text)
		}
		return indent + st.del.Render("✕ ") + st.faint.Render(text)
	}
	check := func(f int, on bool, text string) string {
		box := st.faint.Render("[ ]")
		if on {
			box = st.accent.Bold(true).Render("[x]")
		}
		s := st.text
		if c.focus == f {
			s = st.accent.Bold(true)
		}
		return box + " " + s.Render(text)
	}

	parts := []string{st.bold.Foreground(st.p.accent).Render("New worktree"), ""}

	for i := range c.inputs {
		c.inputs[i].SetWidth(inputW)
	}
	parts = append(parts, label(fieldBranch, "Branch")+c.inputs[fieldBranch].View())
	if text, ok := m.branchStatus(); strings.TrimSpace(c.inputs[fieldBranch].Value()) != "" {
		parts = append(parts, status(text, ok))
	}
	if _, kind, _ := m.resolveBranch(); kind == git.BranchNew {
		parts = append(parts, label(fieldFrom, "From")+c.inputs[fieldFrom].View())
	} else {
		parts = append(parts, label(fieldFrom, "From")+st.faint.Render("the branch's own commits"))
	}
	parts = append(parts, label(fieldFolder, "Folder")+c.inputs[fieldFolder].View())
	if text, ok := m.folderStatus(); !ok && strings.TrimSpace(c.inputs[fieldFolder].Value()) != "" {
		parts = append(parts, status(text, ok))
	}

	var opts []string
	if len(c.copyFiles) > 0 {
		opts = append(opts, check(fieldCopy, c.copy, "Copy "+strings.Join(c.copyFiles, ", ")))
	}
	if m.cfg.New.Setup != "" {
		opts = append(opts, check(fieldSetup, c.setup, "Run setup: "+m.cfg.New.Setup))
	}
	if m.opener.Editor != "" {
		opts = append(opts, check(fieldEditor, c.editor, "Open in "+m.editorName()))
	}
	if len(opts) > 0 {
		parts = append(parts, "")
		parts = append(parts, opts...)
	}

	parts = append(parts, "")
	switch {
	case c.running:
		parts = append(parts, st.accent.Render(spinnerFrames[m.frame%len(spinnerFrames)])+st.muted.Render(" Creating worktree…"))
	case c.err != "":
		parts = append(parts, st.del.Width(w).Render("✕ "+c.err))
	case c.loadErr != nil:
		parts = append(parts, st.warn.Render("Couldn't list branches: ")+st.faint.Render(c.loadErr.Error()))
	}
	parts = append(parts, m.hints([][2]string{{"enter", "create"}, {"tab", "next"}, {"space", "toggle"}, {"esc", "cancel"}}))

	var lines []string
	for _, l := range strings.Split(strings.Join(parts, "\n"), "\n") {
		lines = append(lines, ansi.Truncate(l, w, "…"))
	}
	padY := 1
	if len(lines)+4 > m.height-2 {
		padY = 0
	}
	return st.modal.Padding(padY, 3).Render(strings.Join(lines, "\n"))
}
