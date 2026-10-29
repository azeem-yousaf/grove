package ui

import (
	"errors"
	"strings"
	"testing"

	"github.com/azeem-yousaf/grove/internal/agent"
	"github.com/azeem-yousaf/grove/internal/plan"
	"github.com/azeem-yousaf/grove/internal/store"
)

// The planner's standing rules go to a Claude conversation once; Codex,
// a new conversation or changed rules get them again.
func TestPlannerGetsItsRulesOnce(t *testing.T) {
	const full, short = "Approve a command when it is safe", "as before"
	for _, groupID := range []string{"", "g"} {
		m, p, fake := groupedPlan(t)
		p.Orchestrated = true
		m.cfg.Planner.MaxDecisions = 100
		chat := plan.ChatID
		if groupID != "" {
			chat = groupID
		}
		ask := func(session string) string {
			t.Helper()
			fake.turns = append(fake.turns, []agent.Event{{Kind: agent.EvDone, Session: session}})
			n := len(fake.reqs)
			cmd, sent := m.orchestrateOne([]orchItem{{kind: orchGroup, plan: p.ID, group: chat, lines: []string{"you: hi"}, urgent: true}})
			if !sent {
				t.Fatalf("group %q: the planner wasn't asked", groupID)
			}
			pump(t, m, cmd)
			if len(fake.reqs) != n+1 {
				t.Fatalf("group %q: %d requests, want %d", groupID, len(fake.reqs), n+1)
			}
			return fake.reqs[n].Message
		}
		gets := func(when, msg string, wantFull bool) {
			t.Helper()
			if strings.Contains(msg, full) != wantFull || strings.Contains(msg, short) == wantFull || !strings.Contains(msg, "you: hi") {
				t.Errorf("group %q, %s: full rules %v, got:\n%s", groupID, when, wantFull, msg)
			}
		}

		gets("first turn", ask("s1"), true)
		gets("second turn", ask("s1"), false)

		// A new conversation hasn't seen them.
		gets("before the new session", ask("s2"), false)
		gets("new session", ask("s2"), true)

		// Control tools change the rules.
		m.store = store.Open(t.TempDir())
		gets("control tools", ask("s2"), true)
		gets("after control tools", ask("s2"), false)

		// Restored after a restart, the conversation still has them.
		s := m.plannerFor(p.ID, groupID)
		if r := restore(s.saved(), true, true); r.rulesSent != s.rulesSent || r.rulesSent == "" {
			t.Errorf("group %q: restored rules %q, want %q", groupID, r.rulesSent, s.rulesSent)
		}

		// A failed turn may not have taken them in.
		fake.turns = append(fake.turns, []agent.Event{{Kind: agent.EvDone, Session: "s2", Err: errors.New("cut off")}})
		ask("s2")
		gets("after a failed turn", ask("s2"), true)

		// Codex starts fresh every turn.
		s.kind = agent.Codex
		gets("codex", ask(""), true)
		gets("codex again", ask(""), true)
	}
}
