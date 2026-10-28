// Package config loads grove's optional TOML settings file.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is grove's user configuration. Every field is optional.
type Config struct {
	// Base is the branch worktrees are compared against. Empty means detect
	// it from origin/HEAD, then main or master.
	Base string `toml:"base"`
	// Mode is "branch" or "uncommitted".
	Mode string `toml:"mode"`
	// Refresh is how often grove rescans while the terminal is focused.
	Refresh Duration `toml:"refresh"`
	// Editor opens a whole worktree (key e). Empty means detect one on PATH.
	Editor string `toml:"editor"`
	// Theme is "auto", "dark" or "light".
	Theme string `toml:"theme"`
	// Open overrides the program used for a file extension, for example
	// {".ts" = "code"}. Use "default" for the system default program.
	Open map[string]string `toml:"open"`
	// New sets up worktrees created with key n.
	New NewWorktree `toml:"new"`
	// AI chooses the assistant used for each worktree's agent (key a).
	AI AI `toml:"ai"`
}

// AI configures the per-worktree assistant. grove drives a command-line
// agent that is already installed and signed in; it never handles API keys.
type AI struct {
	// Agent is "claude" or "codex". Empty means ask the first time.
	Agent string `toml:"agent"`
	// Model is passed to the agent; empty uses the agent's default.
	Model string `toml:"model"`
}

// NewWorktree configures creating worktrees.
type NewWorktree struct {
	// Copy lists files or folders, relative to the repository root, copied
	// from the main worktree into a new one: things git ignores, like .env.
	Copy []string `toml:"copy"`
	// Setup is a command run in the new worktree, such as "npm install".
	Setup string `toml:"setup"`
}

// Duration is a time.Duration written as a string such as "3s".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Default returns the built-in settings.
func Default() Config {
	return Config{
		Mode:    "branch",
		Refresh: Duration{3 * time.Second},
		Theme:   "auto",
		Open:    map[string]string{},
		New:     NewWorktree{Copy: []string{".env", ".env.local"}},
	}
}

// Path is where grove looks for its config file.
func Path() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "grove", "config.toml"), nil
}

// Load reads the config file, falling back to defaults when it is missing.
func Load() (Config, error) {
	cfg := Default()
	path, err := Path()
	if err != nil {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if _, err := toml.Decode(string(b), &cfg); err != nil {
		return Default(), fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Refresh.Duration < 500*time.Millisecond {
		cfg.Refresh.Duration = 500 * time.Millisecond
	}
	normalized := make(map[string]string, len(cfg.Open))
	for ext, cmd := range cfg.Open {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		normalized[ext] = strings.TrimSpace(cmd)
	}
	cfg.Open = normalized
	return cfg, nil
}

// WriteDefault creates a commented config file if none exists and returns
// its path.
func WriteDefault() (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return path, fs.ErrExist
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(Template), 0o644)
}

// SaveAgent records the chosen assistant under [ai] agent, keeping the
// rest of the file (and its comments) as it is. It creates the file from
// the template when there is none.
func SaveAgent(agent string) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if _, err := WriteDefault(); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(setKey(string(b), "ai", "agent", fmt.Sprintf("%q", agent))), 0o644)
}

// setKey sets key = value inside [section] of a TOML document, replacing an
// existing assignment or adding one, and adding the section if needed.
func setKey(doc, section, key, value string) string {
	nl := "\n"
	if strings.Contains(doc, "\r\n") {
		nl = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	line := key + " = " + value
	start := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "["+section+"]" {
			start = i
			continue
		}
		if start < 0 {
			continue
		}
		if strings.HasPrefix(t, "[") {
			// End of the section without the key: insert before the next one.
			lines = append(lines[:i], append([]string{line, ""}, lines[i:]...)...)
			return strings.Join(lines, nl)
		}
		if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == key {
			lines[i] = line
			return strings.Join(lines, nl)
		}
	}
	if start >= 0 {
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, line, "")
		return strings.Join(lines, nl)
	}
	out := strings.TrimRight(strings.Join(lines, nl), "\r\n")
	return out + nl + nl + "[" + section + "]" + nl + line + nl
}

// Template is the commented starter config written by `grove --init-config`.
const Template = `# grove configuration
# Every setting is optional. Delete anything you don't need.

# Branch that worktrees are compared against.
# Empty = auto-detect (origin/HEAD, then main, then master).
base = ""

# "branch"      everything a worktree changed since it left the base branch
# "uncommitted" only work that isn't committed yet
mode = "branch"

# How often to rescan while the terminal is focused.
refresh = "3s"

# Program used to open a whole worktree folder (key: e).
# Empty = auto-detect VS Code, Cursor, Zed or Sublime Text on PATH.
editor = ""

# "auto", "dark" or "light"
theme = "auto"

# New worktrees (key: n).
[new]
# Files and folders git ignores that a new worktree needs, copied from the
# main worktree. Glob patterns, relative to the repository root.
copy = [".env", ".env.local"]
# Command run in the new worktree after it's created. Empty = none.
setup = ""

# The assistant each worktree's agent uses (key: a). grove runs a tool that
# is already installed and signed in on this PC.
[ai]
# "claude" (Claude Code) or "codex". Empty = ask when both are installed.
agent = ""
# Model passed to the agent. Empty = the agent's own default.
model = ""

# Files open with their system default program (key: enter).
# Override that per extension here. "default" means the system default.
[open]
# ".ts"  = "code"
# ".log" = "notepad"
`
