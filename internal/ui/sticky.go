package ui

import (
	"runtime"
	"slices"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/gitpolicy"
)

// Sticky approvals: a plan's task agents may run the plan's own checks
// without asking, and the planner may approve a command's family ("go
// test" with any arguments) rather than the exact line. Both only ever
// cover commands gitpolicy calls NotGit: git is judged by the agent hook
// and the guard whatever Claude's rules say. The planner's own read-only
// session never gets either.

// checkShells are the tools a pre-approved check is allowed for.
func checkShells() []string {
	if runtime.GOOS == "windows" {
		return []string{"Bash", "PowerShell"}
	}
	return []string{"Bash"}
}

// planChecks are the allow rules for the test command of the plan s's
// task belongs to: each of its commands' family where it has one, else
// that exact command.
func (m *Model) planChecks(s *session) []string {
	if s.planner {
		return nil
	}
	p, t := m.taskFor(s.path)
	if t == nil {
		return nil
	}
	var rules []string
	for _, tool := range checkShells() {
		for _, c := range agent.CommandParts(m.testCommand(p)) {
			if !m.notGit(s, tool, c) {
				continue
			}
			r, ok := agent.ClaudePrefixRule(tool, c)
			if !ok {
				r = agent.ClaudeRule(tool, c)
			}
			if !slices.Contains(rules, r) {
				rules = append(rules, r)
			}
		}
	}
	return rules
}

// allowFor is every rule s's agent runs with: the plan's checks and what
// was approved.
func (m *Model) allowFor(s *session) []string {
	rules := m.planChecks(s)
	for _, r := range s.allow {
		if !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	return rules
}

// familyRule is the rule for an approval of command's family: one command
// with a program and subcommand that isn't git or an interpreter.
func (m *Model) familyRule(s *session, tool, command string) (string, bool) {
	if s.planner || len(agent.CommandParts(command)) != 1 || !m.notGit(s, tool, command) {
		return "", false
	}
	return agent.ClaudePrefixRule(tool, command)
}

// notGit reports whether gitpolicy leaves command to the normal approvals.
func (m *Model) notGit(s *session, tool, command string) bool {
	return m.classifyFor(s, shellOf(tool), command).Verdict == gitpolicy.NotGit
}
