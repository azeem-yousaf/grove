// Package agenthook checks an agent's shell commands against grove's git
// policy before they run. grove writes each agent's scope to policy.json,
// and commands the user or the planner approved to grants.json, both in
// grove's control folder (internal/control). Claude Code runs `grove
// agent-hook` before every shell command (a PreToolUse hook), which reads
// them and allows the command, asks for it, or leaves it to Claude's normal
// permissions.
package agenthook

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/azeem-yousaf/grove/internal/fslock"
	"github.com/azeem-yousaf/grove/internal/git"
	"github.com/azeem-yousaf/grove/internal/gitpolicy"
)

// PolicyVersion is the Policy.Version this grove writes.
const PolicyVersion = 1

// Policy is every agent's scope, as grove's UI last worked it out.
type Policy struct {
	Version int                   `json:"version"`
	Agents  map[string]AgentScope `json:"agents"` // by git.PathKey(worktree)
}

// AgentScope is what one agent's git commands may touch.
type AgentScope struct {
	gitpolicy.Scope
	Planner bool `json:"planner,omitempty"`
}

// Scope returns the scope of the agent working in worktree.
func (p Policy) Scope(worktree string) (AgentScope, bool) {
	s, ok := p.Agents[git.PathKey(worktree)]
	return s, ok
}

// Set records the scope of the agent working in s.Worktree.
func (p *Policy) Set(s AgentScope) {
	if p.Agents == nil {
		p.Agents = map[string]AgentScope{}
	}
	p.Agents[git.PathKey(s.Worktree)] = s
}

// Grant is a command the user or the planner approved for an agent; the
// hook allows it once more.
type Grant struct {
	ID       string    `json:"id"`
	Worktree string    `json:"worktree"`
	Command  string    `json:"command"`
	Time     time.Time `json:"time"`
}

// GrantTTL is how long an unused grant lasts. The agent runs an approved
// command again as soon as its turn resumes.
const GrantTTL = 10 * time.Minute

func policyFile(dir string) string { return filepath.Join(dir, "policy.json") }
func grantsFile(dir string) string { return filepath.Join(dir, "grants.json") }

// WritePolicy replaces policy.json.
func WritePolicy(dir string, p Policy) error {
	if p.Version == 0 {
		p.Version = PolicyVersion
	}
	return writeJSON(dir, policyFile(dir), p)
}

// ReadPolicy loads what WritePolicy saved.
func ReadPolicy(dir string) (Policy, error) {
	var p Policy
	b, err := os.ReadFile(policyFile(dir))
	if err != nil {
		return p, err
	}
	return p, json.Unmarshal(b, &p)
}

// writeJSON replaces path through a temporary file, so a reader never sees
// half of it.
func writeJSON(dir, path string, v any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	unlock, err := fslock.Lock(filepath.Join(dir, "grants.lock"))
	if errors.Is(err, fslock.ErrBusy) {
		return nil, errors.New("grove's approved commands are locked by another program")
	}
	return unlock, err
}

// readGrants returns the grants that haven't expired. A missing or damaged
// file holds none.
func readGrants(dir string, now time.Time) []Grant {
	var all, out []Grant
	b, err := os.ReadFile(grantsFile(dir))
	if err != nil || json.Unmarshal(b, &all) != nil {
		return nil
	}
	for _, g := range all {
		if now.Sub(g.Time) < GrantTTL {
			out = append(out, g)
		}
	}
	return out
}

// AddGrant records g, filling in its ID and Time when empty.
func AddGrant(dir string, g Grant) error {
	if g.ID == "" {
		b := make([]byte, 6)
		rand.Read(b)
		g.ID = hex.EncodeToString(b)
	}
	if g.Time.IsZero() {
		g.Time = time.Now()
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	return writeJSON(dir, grantsFile(dir), append(readGrants(dir, time.Now()), g))
}

// TakeGrant finds and removes a grant for command in worktree. Commands
// match after collapsing whitespace.
func TakeGrant(dir, worktree, command string) (Grant, bool) {
	if _, err := os.Stat(grantsFile(dir)); err != nil {
		return Grant{}, false // nothing granted; don't take the lock
	}
	unlock, err := lock(dir)
	if err != nil {
		return Grant{}, false
	}
	defer unlock()
	gs := readGrants(dir, time.Now())
	want := normalize(command)
	for i, g := range gs {
		if git.SamePath(g.Worktree, worktree) && normalize(g.Command) == want {
			if writeJSON(dir, grantsFile(dir), append(gs[:i:i], gs[i+1:]...)) != nil {
				return Grant{}, false // couldn't use it up, so don't use it
			}
			return g, true
		}
	}
	return Grant{}, false
}

func normalize(cmd string) string { return strings.Join(strings.Fields(cmd), " ") }
