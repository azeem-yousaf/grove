package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os/exec"
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

type gitDoneMsg struct {
	name, line string
	code       int
	err        error
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
		return tea.Quit
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
	args := splitCommand(line)
	run := &gitRun{
		dir:   st.Path,
		args:  args,
		title: m.st.pillAccent.Render("grove") + " " + m.st.bold.Render(st.Name()) + m.st.faint.Render("  "+st.Path),
		cmd:   m.st.accent.Render("$ git ") + m.st.text.Render(line),
		st:    m.st,
	}
	name := st.Name()
	return tea.Exec(run, func(err error) tea.Msg {
		if err == nil {
			err = run.err
		}
		return gitDoneMsg{name: name, line: line, code: run.code, err: err}
	})
}

func (m *Model) onGitDone(msg gitDoneMsg) tea.Cmd {
	var note tea.Cmd
	switch {
	case msg.err != nil:
		note = m.notify(fmt.Sprintf("git %s: %v", msg.line, msg.err), toastErr)
	case msg.code != 0:
		note = m.notify(fmt.Sprintf("git %s exited with %d in %s", msg.line, msg.code, msg.name), toastErr)
	default:
		note = m.notify(fmt.Sprintf("git %s · done in %s", msg.line, msg.name), toastOK)
	}
	// Whatever git did, the worktrees may have changed.
	return tea.Batch(note, m.startScan())
}

// gitRun is a tea.ExecCommand: it frames git's output and pauses at the end.
type gitRun struct {
	dir, title, cmd string
	args            []string
	st              styles

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

	c := exec.Command("git", g.args...)
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
