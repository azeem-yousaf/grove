package agenthook

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/azeem-yousaf/grove/internal/gitpolicy"
)

// Input is the part of Claude Code's PreToolUse hook input grove reads.
type Input struct {
	Tool      string `json:"tool_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
}

// Output is a PreToolUse hook's answer. No output at all leaves the command
// to Claude's normal permissions.
type Output struct {
	Hook struct {
		Event    string `json:"hookEventName"`
		Decision string `json:"permissionDecision"` // "allow" or "ask"
		Reason   string `json:"permissionDecisionReason,omitempty"`
	} `json:"hookSpecificOutput"`
}

// Hook answers Claude Code's PreToolUse hook for the agent working in
// worktree, with grove's control folder dir.
type Hook struct {
	Dir, Worktree string
	// Lookup answers gitpolicy's questions about the repository in dir;
	// gitpolicy.GitLookup when nil.
	Lookup func(ctx context.Context, dir string) gitpolicy.Lookup
}

// timeout bounds the git questions; Claude Code gives the hook 30 s.
const timeout = 20 * time.Second

// Run reads the hook input from in and writes the decision, if any, to out.
func (h Hook) Run(in io.Reader, out io.Writer) error {
	d, reason, ok := h.decide(in)
	if !ok {
		return nil
	}
	var o Output
	o.Hook.Event, o.Hook.Decision, o.Hook.Reason = "PreToolUse", d, reason
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = out.Write(append(b, '\n'))
	return err
}

// decide returns the permission decision, or ok false to say nothing.
func (h Hook) decide(r io.Reader) (decision, reason string, ok bool) {
	var in Input
	if err := json.NewDecoder(io.LimitReader(r, 4<<20)).Decode(&in); err != nil {
		return "ask", "grove couldn't read the command to check it", true
	}
	var sh gitpolicy.Shell
	switch in.Tool {
	case "Bash":
		sh = gitpolicy.Bash
	case "PowerShell":
		sh = gitpolicy.PowerShell
	default:
		return "", "", false
	}
	if _, ok := TakeGrant(h.Dir, h.Worktree, in.ToolInput.Command); ok {
		return "allow", "approved in grove", true
	}
	var scope AgentScope
	known := false
	if p, err := ReadPolicy(h.Dir); err == nil {
		scope, known = p.Scope(h.Worktree)
	}
	if !known {
		// Without grove's word on this agent, every git command asks.
		scope = AgentScope{}
	}
	if scope.Worktree == "" {
		scope.Worktree = h.Worktree
	}
	// The planner's worktree is a name for it, not a folder; it works in
	// scope.Dir.
	if in.Cwd != "" {
		scope.Dir = in.Cwd
	}
	cwd := scope.Dir
	if cwd == "" {
		cwd = scope.Worktree
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	lookup := h.Lookup
	if lookup == nil {
		lookup = gitpolicy.GitLookup
	}
	dec := gitpolicy.Classify(in.ToolInput.Command, sh, scope.Scope, lookup(ctx, cwd))
	switch {
	case dec.Verdict == gitpolicy.NotGit:
		return "", "", false
	case !known:
		return "ask", "grove has no git policy for this worktree", true
	case dec.Verdict == gitpolicy.Allow:
		return "allow", dec.Reason, true
	}
	return "ask", dec.Reason, true
}
