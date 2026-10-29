package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/azeem-yousaf/grove/internal/usage"
)

// codexArgs builds a `codex exec` run. The workspace-write sandbox lets
// Codex edit and run commands inside the working folder only; a linked
// worktree's git data lives outside it, so commits and checkouts fail by
// design. Each turn is a fresh run with the conversation so far replayed
// in the prompt, which works on every Codex version.
func codexArgs(req Request) (args []string, input string) {
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
		for _, t := range req.History {
			fmt.Fprintf(&in, "User: %s\n\nYou: %s\n\n", t.User, t.Agent)
		}
	}
	in.WriteString("## Request\n\n")
	in.WriteString(req.Message)
	return args, in.String()
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
