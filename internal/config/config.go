// Package config loads grove's optional TOML settings file.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	// Planner configures plans: requirements split into tasks (key P).
	Planner Planner `toml:"planner"`
	// Conflicts configures predicting merge conflicts between worktrees.
	Conflicts Conflicts `toml:"conflicts"`
	// Notify configures telling the user about agents while grove's
	// terminal isn't focused.
	Notify Notify `toml:"notify"`
	// Git configures what agents may do with git on their own.
	Git Git `toml:"git"`
}

// Git configures agents' git autonomy: routine git on their own branches
// runs without asking; protected branches only change with the user's
// approval.
type Git struct {
	// Protected lists more branches agents never change on their own: names
	// or globs such as "release/*". main, master and the base branch are
	// always protected.
	Protected []string `toml:"protected"`
}

// ProtectedWith is Protected joined with main, master and base (the
// detected default branch), without duplicates or blanks.
func (g Git) ProtectedWith(base string) []string {
	out := []string{"main", "master"}
	for _, p := range append([]string{base}, g.Protected...) {
		p = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p), "refs/heads/"))
		if p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// Notify configures notifications sent while grove's terminal isn't
// focused: an agent finished, needs the user or failed, a plan is ready or
// done, or worktrees start to conflict.
type Notify struct {
	// Desktop shows a desktop notification. Unset means on.
	Desktop *bool `toml:"desktop"`
	// Bell also rings the terminal bell, which many terminals show by
	// flashing the tab or the taskbar button.
	Bell bool `toml:"bell"`
}

// DesktopOn is Desktop with the default applied.
func (n Notify) DesktopOn() bool { return n.Desktop == nil || *n.Desktop }

// Conflicts configures conflict prediction: test-merging worktrees that
// change the same files to see whether they would really conflict.
type Conflicts struct {
	// Predict turns prediction on or off. Unset means on.
	Predict *bool `toml:"predict"`
}

// Enabled is Predict with the default applied.
func (c Conflicts) Enabled() bool { return c.Predict == nil || *c.Predict }

// Planner configures the planner and the agents it starts.
type Planner struct {
	// MaxAgents is how many task agents may work at once; more tasks wait
	// for a free slot. 0 or less means the default, 3.
	MaxAgents int `toml:"max_agents"`
	// MaxWakeups is how many times in a row grove wakes idle agents of one
	// group to answer group messages before it waits for the user to post.
	// 0 or less means the default, 6.
	MaxWakeups int `toml:"max_wakeups"`
	// MaxDecisions is how many turns in a row the planner may take for the
	// plans it watches before it waits for the user to say something. 0 or
	// less means the default, 20.
	MaxDecisions int `toml:"max_decisions"`
	// Watcher is who watches a running plan's agents and answers them:
	// "you", or "planner" for the planner to approve their commands, answer
	// their questions and follow their group chats. Empty means ask before
	// each plan.
	Watcher string `toml:"watcher"`
	// Test is the command grove runs in a plan's worktree after the planner
	// merges the plan, such as "go test ./..." or "npm test", through sh
	// (cmd.exe on Windows). Empty means the planner's own suggestion for
	// the plan, if any.
	Test string `toml:"test"`
	// TestMinutes is how long the test command may run before it is
	// stopped. 0 or less means the default, 20.
	TestMinutes int `toml:"test_minutes"`
	// PullRequests has grove push a merged plan's branch and open a pull
	// request into the plan's base once its tests pass. Unset means on.
	PullRequests *bool `toml:"pull_requests"`
}

// OpenPRs is PullRequests with the default applied.
func (p Planner) OpenPRs() bool { return p.PullRequests == nil || *p.PullRequests }

// Watchers are the values Planner.Watcher may take.
const (
	WatchYou     = "you"
	WatchPlanner = "planner"
)

// Wakeups is MaxWakeups with the default applied.
func (p Planner) Wakeups() int {
	if p.MaxWakeups <= 0 {
		return 6
	}
	return p.MaxWakeups
}

// Decisions is MaxDecisions with the default applied.
func (p Planner) Decisions() int {
	if p.MaxDecisions <= 0 {
		return 20
	}
	return p.MaxDecisions
}

// TestTimeout is TestMinutes with the default applied.
func (p Planner) TestTimeout() time.Duration {
	if p.TestMinutes <= 0 {
		return 20 * time.Minute
	}
	return time.Duration(p.TestMinutes) * time.Minute
}

// Slots is MaxAgents with the default applied.
func (p Planner) Slots() int {
	if p.MaxAgents <= 0 {
		return 3
	}
	return p.MaxAgents
}

// AI configures the per-worktree assistant. grove drives a command-line
// agent that is already installed and signed in; it never handles API keys.
type AI struct {
	// Agent is "claude" or "codex". Empty means ask the first time.
	Agent string `toml:"agent"`
	// ClaudeModel and CodexModel are passed to that agent; empty uses the
	// agent's own default (for Claude Code, what /model is set to there).
	// /model in grove's chat changes them.
	ClaudeModel string `toml:"claude_model"`
	CodexModel  string `toml:"codex_model"`
	// Model is the older setting for both agents, used when the agent's
	// own key is empty.
	Model string `toml:"model"`
}

// ModelFor is the model for agent ("claude" or "codex").
func (a AI) ModelFor(agent string) string {
	m := map[string]string{"claude": a.ClaudeModel, "codex": a.CodexModel}[agent]
	if m == "" {
		return a.Model
	}
	return m
}

// NewWorktree configures creating worktrees.
type NewWorktree struct {
	// Copy lists files or folders, relative to the repository root, copied
	// from the main worktree into a new one: things git ignores, like .env.
	Copy []string `toml:"copy"`
	// Setup is a command run in the new worktree, such as "npm install".
	Setup string `toml:"setup"`
	// OpenEditor opens the new worktree in the configured editor after creation.
	OpenEditor bool `toml:"open_editor"`
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
func SaveAgent(agent string) error { return saveAI("agent", agent) }

// SaveModel records agent's model ("" for its default) under [ai], the
// same way.
func SaveModel(agent, model string) error { return saveAI(agent+"_model", model) }

// SaveWatcher records who watches running plans under [planner] watcher.
func SaveWatcher(watcher string) error { return saveKey("planner", "watcher", watcher) }

func saveAI(key, value string) error { return saveKey("ai", key, value) }

func saveKey(section, key, value string) error {
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
	return os.WriteFile(path, []byte(setKey(string(b), section, key, fmt.Sprintf("%q", value))), 0o644)
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
# Open the new worktree in the configured editor after creation.
open_editor = false

# The assistant each worktree's agent uses (key: a). grove runs a tool that
# is already installed and signed in on this PC.
[ai]
# "claude" (Claude Code) or "codex". Empty = ask when both are installed.
agent = ""
# Model for each agent, set with /model in the chat. A name or an alias
# such as "opus" or "sonnet". Empty = the agent's own default (for Claude
# Code, whatever /model is set to there).
claude_model = ""
codex_model = ""

# The planner (key: P) splits requirements into tasks, each with its own
# worktree and agent.
[planner]
# How many task agents may work at the same time; the rest wait.
max_agents = 3
# How many times in a row grove wakes idle agents of a group to answer each
# other before it waits for you to post in the group chat.
max_wakeups = 6
# How many turns in a row a planner may take for a plan it watches without
# the plan moving forward before it waits for you to say something.
max_decisions = 20
# Who watches a running plan's agents and answers them: "you", or "planner"
# to put the planner (and a planner per group) in charge: it approves their
# commands (all but destructive git), answers their questions, helps the
# ones that get stuck, merges and tests the plan and opens a pull request,
# and follows your review of it. Empty = ask before each plan.
watcher = ""
# The command grove runs in a plan's worktree after the planner merges the
# plan, e.g. "go test ./..." or "npm test" (through sh, or cmd.exe on
# Windows). Failures go back to the tasks' agents before you are asked.
# Empty = the planner's suggestion for each plan, if it made one.
test = ""
# Minutes the test command may run before it is stopped.
test_minutes = 20
# Once a merged plan's tests pass, push its plan/<name> branch and open a
# pull request into the base branch with the GitHub CLI (gh). While the
# planner is in charge, it follows the pull request's reviews, and the plan
# closes once the pull request is merged.
pull_requests = true

# What agents may do with git on their own. Routine git on their own
# feature branch (commit, branch, merge, rebase of unpushed work, push of
# their branch) just runs; anything that changes a protected branch, or
# could lose work, waits for you.
[git]
# Branches agents never change without you, as names or globs. main,
# master and the base branch are always protected.
protected = ["release/*"]

# Files changed in more than one worktree are test-merged in git's object
# store (nothing on disk changes) to show which would really conflict.
# Needs git 2.38 or later.
[conflicts]
# Set to false to turn it off, for example in very large repositories.
predict = true

# While grove's terminal isn't focused, tell you when an agent finishes,
# needs you or fails, a plan is ready or done, or worktrees start to
# conflict. Needs a terminal that reports focus (Windows Terminal, iTerm2,
# WezTerm, kitty, Ghostty and most others).
[notify]
# A desktop notification (on Linux this needs notify-send).
desktop = true
# Also ring the terminal bell; many terminals flash the tab or taskbar.
bell = false

# Files open with their system default program (key: enter).
# Override that per extension here. "default" means the system default.
[open]
# ".ts"  = "code"
# ".log" = "notepad"
`
