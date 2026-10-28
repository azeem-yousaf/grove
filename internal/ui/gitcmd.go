package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/azeem-yousaf/grove/internal/git"
)

// gitPrompt is the ":" command line that runs git in one worktree.
type gitPrompt struct {
	input   textinput.Model
	active  bool
	target  git.State
	history []string
	hist    int // position while browsing history; len(history) = new line
	draft   string
}

// gitDoneMsg reports a command that ran with the terminal handed over.
type gitDoneMsg struct {
	name    string // worktree it ran in
	display string // the command as shown, e.g. "git status"
	code    int
	err     error
	then    func() tea.Cmd // runs afterwards, whatever the outcome
}

func newGitPrompt() gitPrompt {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = `status · commit -m "message" · pull --rebase · log --oneline`
	return gitPrompt{input: ti}
}

// openGitPrompt starts typing a git command for the selected worktree.
func (m *Model) openGitPrompt() tea.Cmd {
	r, ok := m.selected()
	if !ok {
		return nil
	}
	st := m.snap.States[r.wt]
	if st.Prunable || st.Bare {
		return m.notify("That worktree has no folder to run git in", toastInfo)
	}
	g := &m.gitCmd
	g.active, g.target = true, st
	g.hist, g.draft = len(g.history), ""
	g.input.SetValue("")
	return g.input.Focus()
}

func (m *Model) onGitPromptKey(msg tea.KeyPressMsg) tea.Cmd {
	g := &m.gitCmd
	switch msg.String() {
	case "esc":
		g.active = false
		g.input.Blur()
		return nil
	case "ctrl+c":
		return m.quitNow()
	case "up":
		if g.hist > 0 {
			if g.hist == len(g.history) {
				g.draft = g.input.Value()
			}
			g.hist--
			g.input.SetValue(g.history[g.hist])
			g.input.CursorEnd()
		}
		return nil
	case "down":
		if g.hist < len(g.history) {
			g.hist++
			if g.hist == len(g.history) {
				g.input.SetValue(g.draft)
			} else {
				g.input.SetValue(g.history[g.hist])
			}
			g.input.CursorEnd()
		}
		return nil
	case "enter":
		line := strings.TrimSpace(g.input.Value())
		line = strings.TrimSpace(strings.TrimPrefix(line, "git "))
		g.active = false
		g.input.Blur()
		if line == "" || line == "git" {
			return nil
		}
		if n := len(g.history); n == 0 || g.history[n-1] != line {
			g.history = append(g.history, line)
		}
		return m.runGit(g.target, line)
	}
	var cmd tea.Cmd
	g.input, cmd = g.input.Update(msg)
	return cmd
}

// runGit hands the terminal to git so pagers, editors and prompts work,
// then waits for enter so the output can be read before grove redraws.
func (m *Model) runGit(st git.State, line string) tea.Cmd {
	return m.runInTerminal(st.Name(), st.Path, "git", splitCommand(line), "git "+line)
}

// runInTerminal runs prog in dir with the terminal handed over.
func (m *Model) runInTerminal(name, dir, prog string, args []string, display string) tea.Cmd {
	return m.runInTerminalThen(name, dir, prog, args, display, nil)
}

// runInTerminalThen is runInTerminal followed by then, if not nil.
func (m *Model) runInTerminalThen(name, dir, prog string, args []string, display string, then func() tea.Cmd) tea.Cmd {
	run := &gitRun{
		dir:   dir,
		prog:  prog,
		args:  args,
		title: m.st.pillAccent.Render("grove") + " " + m.st.bold.Render(name) + m.st.faint.Render("  "+dir),
		cmd:   m.st.accent.Render("$ ") + m.st.text.Render(display),
		st:    m.st,
	}
	return tea.Exec(run, func(err error) tea.Msg {
		if err == nil {
			err = run.err
		}
		return gitDoneMsg{name: name, display: display, code: run.code, err: err, then: then}
	})
}

func (m *Model) onGitDone(msg gitDoneMsg) tea.Cmd {
	var note tea.Cmd
	switch {
	case msg.err != nil:
		note = m.notify(fmt.Sprintf("%s: %v", msg.display, msg.err), toastErr)
	case msg.code != 0:
		note = m.notify(fmt.Sprintf("%s exited with %d in %s", msg.display, msg.code, msg.name), toastErr)
	default:
		note = m.notify(fmt.Sprintf("%s · done in %s", msg.display, msg.name), toastOK)
	}
	// Whatever ran, the worktrees may have changed.
	cmds := []tea.Cmd{note, m.startScan()}
	if msg.then != nil {
		cmds = append(cmds, msg.then())
	}
	return tea.Batch(cmds...)
}

// shellCommand runs a configured command line through the platform shell.
func shellCommand(line string) (prog string, args []string) {
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("pwsh"); err == nil {
			return "pwsh", []string{"-NoProfile", "-Command", line}
		}
		return "powershell", []string{"-NoProfile", "-Command", line}
	}
	return "sh", []string{"-c", line}
}

// gitRun is a tea.ExecCommand: it frames a command's output and pauses at
// the end. prog defaults to git.
type gitRun struct {
	dir, prog, title, cmd string
	args                  []string
	st                    styles

	stdin          io.Reader
	stdout, stderr io.Writer

	code int
	err  error
}

func (g *gitRun) SetStdin(r io.Reader)  { g.stdin = r }
func (g *gitRun) SetStdout(w io.Writer) { g.stdout = w }
func (g *gitRun) SetStderr(w io.Writer) { g.stderr = w }

// Run always returns nil so Bubble Tea restores the screen normally; the
// outcome is kept on g.
func (g *gitRun) Run() error {
	out := g.stdout
	lipgloss.Fprintln(out, "\n"+g.title)
	lipgloss.Fprintln(out, g.cmd+"\n")

	prog := g.prog
	if prog == "" {
		prog = "git"
	}
	c := exec.Command(prog, g.args...)
	c.Dir = g.dir
	c.Stdin, c.Stdout, c.Stderr = g.stdin, out, g.stderr
	err := c.Run()

	var exit *exec.ExitError
	status := g.st.add.Render("✓ done")
	switch {
	case errors.As(err, &exit):
		g.code = exit.ExitCode()
		status = g.st.del.Render(fmt.Sprintf("✕ exit %d", g.code))
	case err != nil:
		g.err = err
		status = g.st.del.Render("✕ " + err.Error())
	}
	lipgloss.Fprintln(out, "\n"+status+g.st.faint.Render("  ·  press enter to return to grove"))
	_, _ = bufio.NewReader(g.stdin).ReadString('\n')
	return nil
}

// splitCommand splits a command line into arguments the way a shell would
// for simple cases: spaces separate, and single or double quotes group.
func splitCommand(s string) []string {
	var (
		args  []string
		cur   strings.Builder
		quote rune
		has   bool
	)
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '"' || r == '\''):
			quote, has = r, true
		case quote == 0 && (r == ' ' || r == '\t'):
			if has {
				args = append(args, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		args = append(args, cur.String())
	}
	return args
}
