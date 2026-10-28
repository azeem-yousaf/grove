// Package open launches files and folders in other programs.
package open

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Opener decides which program opens what.
type Opener struct {
	// Editor opens worktree folders, and files whose extension is mapped to
	// "editor". Empty when none was configured or found.
	Editor string
	// Overrides maps a lower-case extension (".ts") to a command, or to
	// "default" for the system default program.
	Overrides map[string]string
}

// New builds an Opener from the configured editor and overrides. When no
// editor is configured it looks for a common GUI editor on PATH.
func New(editor string, overrides map[string]string) Opener {
	if editor == "" {
		editor = detectEditor()
	}
	o := Opener{Editor: editor, Overrides: map[string]string{}}
	// Windows often registers .ts as an MPEG video stream, so "default
	// program" would launch a media player for TypeScript. Send those to the
	// editor unless the user said otherwise.
	if runtime.GOOS == "windows" && editor != "" {
		for _, ext := range []string{".ts", ".mts", ".cts"} {
			o.Overrides[ext] = editor
		}
	}
	for ext, cmd := range overrides {
		o.Overrides[ext] = cmd
	}
	return o
}

func detectEditor() string {
	for _, name := range []string{"code", "cursor", "zed", "subl"} {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return ""
}

// File opens a file with the program configured for its extension, or the
// system default program.
func (o Opener) File(path string) (via string, err error) {
	ext := strings.ToLower(filepath.Ext(path))
	if cmd, ok := o.Overrides[ext]; ok && cmd != "" && cmd != "default" {
		return commandName(cmd), Run(cmd, path)
	}
	return "default app", Default(path)
}

// Folder opens a worktree folder in the editor, or the file manager when
// there is no editor.
func (o Opener) Folder(path string) (via string, err error) {
	if o.Editor != "" {
		return commandName(o.Editor), Run(o.Editor, path)
	}
	return fileManager, Default(path)
}

// Run starts command (which may include arguments) with path appended, and
// does not wait for it to exit.
func Run(command, path string) error {
	args := splitArgs(command)
	if len(args) == 0 {
		return errors.New("empty command")
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("%s was not found on PATH", args[0])
	}
	cmd := exec.Command(bin, append(args[1:], path)...)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func commandName(command string) string {
	args := splitArgs(command)
	if len(args) == 0 {
		return command
	}
	return strings.TrimSuffix(filepath.Base(args[0]), filepath.Ext(args[0]))
}

// splitArgs splits a command line on spaces, honouring double quotes so
// paths such as "C:\Program Files\Editor\editor.exe" survive.
func splitArgs(s string) []string {
	var (
		args  []string
		cur   strings.Builder
		quote bool
		has   bool
	)
	for _, r := range s {
		switch {
		case r == '"':
			quote = !quote
			has = true
		case r == ' ' && !quote:
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
