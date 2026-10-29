package ui

import (
	"path"
	"slices"
	"strings"
)

// approvalPolicy is what grove's git policy says about a command an agent
// was refused: whether only the user may approve it, and why.
type approvalPolicy struct {
	userOnly    bool     // the planner may not approve it
	reason      string   // short and user-facing, e.g. "merges into protected branch master"
	protected   bool     // it would change a protected branch
	destructive bool     // it could lose work
	refs        []string // branches it would change, for the git guard's grants
}

// gitReads are git subcommands that change nothing.
var gitReads = []string{"status", "diff", "log", "show", "blame", "grep", "ls-files", "rev-parse", "describe", "shortlog", "reflog", "cat-file", "ls-tree", "merge-base", "version", "help"}

// stubPolicy keeps every command that may write with git for the user and
// leaves the rest to whoever watches the plan.
//
// TODO: once internal/gitpolicy is merged, classify with
// gitpolicy.Classify(command, shell of tool, the session's scope, lookup):
// userOnly when the verdict is User or the decision is Protected or
// Destructive.
func stubPolicy(_ *session, _, command string) approvalPolicy {
	fields := strings.FieldsFunc(command, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '&' || r == '|' || r == '(' || r == ')' || r == '`'
	})
	for i, f := range fields {
		name := strings.ToLower(path.Base(strings.ReplaceAll(strings.Trim(f, `"'`), `\`, "/")))
		if name != "git" && name != "git.exe" {
			continue
		}
		sub := ""
		for _, a := range fields[i+1:] {
			if !strings.HasPrefix(a, "-") {
				sub = a
				break
			}
		}
		if !slices.Contains(gitReads, sub) {
			return approvalPolicy{userOnly: true, reason: "runs git commands that may write, which only you approve"}
		}
	}
	return approvalPolicy{}
}

// noGrant records nothing beyond the Claude permission rule. by is byUser
// or byPlanner.
//
// TODO: once internal/agenthook and internal/gitguard are merged, add an
// agenthook grant for the agent and command (so the PreToolUse hook lets
// the rerun through, even inside a compound command), and, when the user
// approved a protected command, a gitguard grant for each of pol.refs.
func noGrant(s *session, tool, command string, pol approvalPolicy, by string) error { return nil }
