package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/azeem-yousaf/grove/internal/usage"
)

// codexArgs builds a `codex exec` run. The workspace-write sandbox lets
// Codex edit and run commands inside the working folder only; a linked
// worktree's git data lives outside it, so commits and checkouts fail by
// design. Each turn is a fresh run with the conversation so far replayed
// in the prompt, within lim, which works on every Codex version.
func codexArgs(req Request, lim Replay) (args []string, input string) {
	sandbox := "workspace-write"
	if req.ReadOnly && !req.Commands {
		sandbox = "read-only"
	}
	args = []string{"exec", "--json", "--sandbox", sandbox}
	// Tool servers go in as config overrides; values are TOML, and JSON
	// strings and arrays of strings are valid TOML.
	for _, s := range req.Tools {
		cmd, _ := json.Marshal(s.Command)
		list, _ := json.Marshal(s.Args)
		args = append(args, "-c", "mcp_servers."+s.Name+".command="+string(cmd), "-c", "mcp_servers."+s.Name+".args="+string(list))
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	args = append(args, "-") // prompt from stdin

	var in strings.Builder
	if req.Context != "" {
		in.WriteString(req.Context)
		in.WriteString("\n\n")
	}
	if len(req.History) > 0 {
		in.WriteString("## Conversation so far\n\n")
		in.WriteString(lim.history(req.History))
	}
	in.WriteString("## Request\n\n")
	in.WriteString(req.Message)
	return args, in.String()
}

// Replay limits how much of the conversation a Codex turn sends again, since
// every turn replays it in full and the cost would otherwise grow each turn.
// Only what is sent changes; the briefing and the message are never cut.
type Replay struct {
	Full   int // newest turns sent whole
	Brief  int // characters an older turn's reply is cut to
	Budget int // characters of history at most; older turns beyond it are left out
}

// DefaultReplay is what NewRunner uses.
var DefaultReplay = Replay{Full: 6, Brief: 300, Budget: 24000}

// history renders turns newest first into the budget: the newest Full whole,
// older ones with the reply cut to Brief. A whole turn that doesn't fit is
// cut too; once one doesn't fit at all, it and every older turn are left
// out, which one line at the top says.
func (lim Replay) history(turns []Turn) string {
	if lim == (Replay{}) {
		lim = DefaultReplay
	}
	var kept []string
	size := 0
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		whole := fmt.Sprintf("User: %s\n\nYou: %s\n\n", t.User, t.Agent)
		cut := fmt.Sprintf("User: %s\n\nYou: %s\n\n", t.User, clip(t.Agent, lim.Brief))
		s := cut
		if len(kept) < lim.Full && size+utf8.RuneCountInString(whole) <= lim.Budget {
			s = whole
		}
		n := utf8.RuneCountInString(s)
		if size+n > lim.Budget {
			break
		}
		kept, size = append(kept, s), size+n
	}
	var b strings.Builder
	if left := len(turns) - len(kept); left > 0 {
		fmt.Fprintf(&b, "(%d earlier %s left out.)\n\n", left, plural(left, "turn", "turns"))
	}
	for i := len(kept) - 1; i >= 0; i-- {
		b.WriteString(kept[i])
	}
	return b.String()
}

// clip cuts s to its first n characters, ending with "…" when it was cut.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n])) + "…"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// codexParser reads `codex exec --json`. Codex reports usage only when a
// turn completes, so there are no live figures; a run that has several
// turns adds them up.
type codexParser struct {
	session  string
	tokens   usage.Tokens
	hasUsage bool // some turn reported usable figures
	err      error
	turned   bool // a turn completed or failed
}

type codexLine struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     codexItem       `json:"item"`
	Usage    json.RawMessage `json:"usage"`
	Error    json.RawMessage `json:"error"`
	Message  string          `json:"message"`
}

type codexItem struct {
	Type     string `json:"type"`
	ItemType string `json:"item_type"` // older builds
	Text     string `json:"text"`
	Command  string `json:"command"`
	Status   string `json:"status"`
	Output   string `json:"aggregated_output"` // command_execution
	ExitCode *int   `json:"exit_code"`         // command_execution
	Changes  []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
}

func (it codexItem) kind() string {
	if it.Type != "" {
		return it.Type
	}
	return it.ItemType
}

func (p *codexParser) line(b []byte) []Event {
	var l codexLine
	if json.Unmarshal(b, &l) != nil {
		return nil
	}
	switch l.Type {
	case "thread.started":
		p.session = l.ThreadID
		return []Event{{Kind: EvSession, Session: l.ThreadID}}
	case "item.started":
		if l.Item.kind() == "command_execution" {
			return []Event{{Kind: EvTool, Tool: "Run", Detail: l.Item.Command}}
		}
	case "item.completed":
		switch l.Item.kind() {
		case "agent_message", "assistant_message":
			if strings.TrimSpace(l.Item.Text) != "" {
				return []Event{{Kind: EvText, Text: l.Item.Text}}
			}
		case "command_execution":
			failed := l.Item.Status == "failed" || l.Item.ExitCode != nil && *l.Item.ExitCode != 0
			return []Event{{Kind: EvOutput, Tool: "Run", Detail: l.Item.Command, Text: tail(l.Item.Output), Failed: failed}}
		case "file_change":
			var evs []Event
			for _, c := range l.Item.Changes {
				evs = append(evs, Event{Kind: EvTool, Tool: "Edit", Detail: c.Path, Path: c.Path})
			}
			return evs
		}
	case "turn.completed":
		p.turned = true
		return p.addUsage(l.Usage)
	case "turn.failed", "error":
		p.turned = true
		if msg := errorText(l.Error, l.Message); msg != "" {
			p.err = errors.New(msg)
		}
		return p.addUsage(l.Usage)
	}
	return nil
}

// addUsage counts a turn's usage, if it gave any, and reports the run's
// total so far.
func (p *codexParser) addUsage(raw json.RawMessage) []Event {
	t, ok := openAIUsage(raw)
	if !ok {
		return nil
	}
	p.tokens, p.hasUsage = p.tokens.Add(t), true
	return []Event{{Kind: EvUsage, Tokens: p.tokens}}
}

// errorText reads an error that may be a string or {"message": ...}.
func errorText(raw json.RawMessage, fallback string) string {
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s
	}
	var o struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &o) == nil && o.Message != "" {
		return o.Message
	}
	return fallback
}

func (p *codexParser) done(exitErr error, stderr string) Event {
	ev := Event{Kind: EvDone, Session: p.session, Err: p.err}
	if p.hasUsage {
		// Codex's own figures, even if the run then failed or was stopped.
		ev.Tokens, ev.TokenStatus = p.tokens, usage.Exact
	}
	if ev.Err == nil && (exitErr != nil || !p.turned) {
		ev.Err = processErr("Codex", exitErr, stderr)
	}
	return ev
}
