// Package gitguard is git's own backstop for protected branches: a hooks
// folder with a reference-transaction hook and a pre-push hook that refuse
// updates to protected branches unless the user granted one. Only processes
// grove starts use it, through core.hooksPath set in their environment
// (Env); nothing in the user's git configuration changes. Every hook of the
// user's own hooks folder is forwarded, so those keep running.
package gitguard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ProbeRef is the ref Verify tries to create; the hook always refuses it.
const ProbeRef = "refs/grove/guard-probe"

// Guard is an installed hooks folder.
type Guard struct {
	Dir string // <store>/hooks, the core.hooksPath of guarded processes
}

// settings is guard.json: what the hooks read when git runs them.
type settings struct {
	Protected []string `json:"protected"`
	Original  string   `json:"original"` // the hooks folder the user's git uses
	Hooks     []string `json:"hooks"`    // hook files Install wrote
}

const (
	settingsFile = "guard.json"
	grantsFile   = "grants.json"
	grantsLock   = "grants.lock"
)

// executable is the grove binary the hooks run; tests replace it.
var executable = os.Executable

// Install writes the guard's hooks folder under storeDir for the
// repository whose common git folder is commonDir. It only rewrites files
// whose content changed, so calling it again is cheap: do so at startup and
// whenever the protected list or the user's own hooks may have changed.
func Install(storeDir, commonDir string, protected []string) (Guard, error) {
	g := Guard{Dir: filepath.Join(storeDir, "hooks")}
	exe, err := executable()
	if err != nil {
		return g, err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	if err := os.MkdirAll(g.Dir, 0o755); err != nil {
		return g, err
	}
	orig := originalHooks(commonDir, g.Dir)

	files := map[string]string{
		"reference-transaction": guardScript("reference-transaction", exe, g.Dir, orig),
		"pre-push":              guardScript("pre-push", exe, g.Dir, orig),
	}
	if entries, err := os.ReadDir(orig); err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || strings.HasSuffix(name, ".sample") || strings.HasPrefix(name, ".") {
				continue
			}
			if _, ok := files[name]; !ok {
				files[name] = forwardScript(orig, name)
			}
		}
	}

	prev := readSettings(g.Dir)
	for _, name := range prev.Hooks {
		if _, ok := files[name]; !ok {
			os.Remove(filepath.Join(g.Dir, name))
		}
	}
	names := make([]string, 0, len(files))
	for name, body := range files {
		names = append(names, name)
		if err := writeIfChanged(filepath.Join(g.Dir, name), []byte(body), 0o755); err != nil {
			return g, err
		}
	}
	slices.Sort(names)
	s := settings{Protected: protected, Original: orig, Hooks: names}
	if s.Protected == nil {
		s.Protected = []string{}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return g, err
	}
	return g, writeIfChanged(filepath.Join(g.Dir, settingsFile), data, 0o644)
}

// originalHooks is the hooks folder the user's git uses for the repository:
// core.hooksPath from its configuration, else <commonDir>/hooks.
func originalHooks(commonDir, guardDir string) string {
	def := filepath.Join(commonDir, "hooks")
	cmd := exec.Command("git", "-C", commonDir, "config", "--type=path", "--get", "core.hooksPath")
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	p := strings.TrimSpace(string(out))
	if err != nil || p == "" {
		return def
	}
	if !filepath.IsAbs(p) {
		// git takes a relative hooksPath relative to where hooks run, the
		// top of the worktree; the main worktree is the closest guess.
		p = filepath.Join(filepath.Dir(commonDir), p)
	}
	if samePath(p, guardDir) {
		return def
	}
	return p
}

func readSettings(dir string) settings {
	var s settings
	if data, err := os.ReadFile(filepath.Join(dir, settingsFile)); err == nil {
		json.Unmarshal(data, &s)
	}
	return s
}

func writeIfChanged(path string, data []byte, perm os.FileMode) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&perm == perm {
			return nil
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp.Name(), perm)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// shQuote quotes s for sh. Paths use forward slashes, which Git for
// Windows' sh and Windows programs both accept.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(filepath.ToSlash(s), "'", `'\''`) + "'"
}

// guardScript runs grove's check for the hook, then the user's own hook of
// the same name with the same arguments and input. The input is kept in a
// temporary file so both can read it. reference-transaction is only
// checked in its "prepared" state; the others go straight to the user's hook.
func guardScript(name, exe, dir, orig string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Written by grove (git-guard). Do not edit; grove rewrites it.\n")
	fmt.Fprintf(&b, "h=%s\n", shQuote(filepath.Join(orig, name)))
	if name == "reference-transaction" {
		b.WriteString("if [ \"$1\" != prepared ]; then\n\tif [ -x \"$h\" ]; then exec \"$h\" \"$@\"; fi\n\tcat >/dev/null\n\texit 0\nfi\n")
	}
	b.WriteString("t=$(mktemp) || exit 1\ncat >\"$t\"\n")
	fmt.Fprintf(&b, "%s git-guard %s --dir %s \"$@\" <\"$t\"\n", shQuote(exe), name, shQuote(dir))
	b.WriteString("s=$?\nif [ $s -eq 0 ] && [ -x \"$h\" ]; then \"$h\" \"$@\" <\"$t\"; s=$?; fi\nrm -f \"$t\"\nexit $s\n")
	return b.String()
}

// forwardScript runs the user's own hook in place of the guard folder's.
func forwardScript(orig, name string) string {
	return "#!/bin/sh\n# Written by grove (git-guard). Do not edit; grove rewrites it.\n" +
		"h=" + shQuote(filepath.Join(orig, name)) + "\n" +
		"if [ -x \"$h\" ]; then exec \"$h\" \"$@\"; fi\nexit 0\n"
}

// Env is what to append to os.Environ() for a process that must run git
// under the guard: core.hooksPath through GIT_CONFIG_COUNT, numbered after
// any GIT_CONFIG_* entries already in the environment. Append it after
// os.Environ(), so its GIT_CONFIG_COUNT is the one that counts.
func (g Guard) Env() []string {
	n := 0
	if v, ok := os.LookupEnv("GIT_CONFIG_COUNT"); ok {
		if c, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && c > 0 {
			n = c
		}
	}
	return []string{
		"GIT_CONFIG_COUNT=" + strconv.Itoa(n+1),
		"GIT_CONFIG_KEY_" + strconv.Itoa(n) + "=core.hooksPath",
		"GIT_CONFIG_VALUE_" + strconv.Itoa(n) + "=" + g.Dir,
	}
}

// Verify proves the guard works in worktree: with Env, creating ProbeRef
// must be refused by the hook. It also reports git too old for the guard
// (reference-transaction needs 2.28, GIT_CONFIG_COUNT 2.31).
func Verify(ctx context.Context, g Guard, worktree string) error {
	out, err := exec.CommandContext(ctx, "git", "version").Output()
	if err != nil {
		return fmt.Errorf("git version: %w", err)
	}
	if !versionAtLeast(string(out), 2, 31) {
		return fmt.Errorf("the git guard needs git 2.31 or newer; this is %s", strings.TrimSpace(string(out)))
	}
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "update-ref", ProbeRef, "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, g.Env()...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		del := exec.CommandContext(ctx, "git", "-C", worktree, "update-ref", "-d", ProbeRef)
		del.Env = os.Environ()
		del.Run()
		return errors.New("the git guard is not active: a test update to a protected ref went through")
	}
	if !strings.Contains(stderr.String(), refusal(ProbeRef)) {
		return fmt.Errorf("the git guard did not answer: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// versionAtLeast reads "git version 2.45.1(.windows.1)" output.
func versionAtLeast(out string, major, minor int) bool {
	f := strings.Fields(out)
	if len(f) < 3 {
		return false
	}
	parts := strings.SplitN(f[2], ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > major || ma == major && mi >= minor
}

func refusal(ref string) string {
	return "grove: " + ref + " is protected; ask the user to approve this in grove"
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if x, err := filepath.EvalSymlinks(a); err == nil {
		a = x
	}
	if x, err := filepath.EvalSymlinks(b); err == nil {
		b = x
	}
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}
