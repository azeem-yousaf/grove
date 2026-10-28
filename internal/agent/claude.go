package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// readOnlyGit are git commands an agent may run without asking; they can't
// change the repository.
var readOnlyGit = []string{"status", "diff", "log", "show", "blame"}

// claudeArgs builds a `claude -p` run. File edits inside the working folder
// are accepted automatically (acceptEdits); anything else that needs
// permission, such as a shell command or a file outside the worktree, is
// denied and reported in the result so grove can ask the user.
func claudeArgs(req Request) (args []string, input string) {
	args = []string{
		"-p", "Do what the request at the end of the piped input asks.",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-mode", "acceptEdits",
	}
	if req.Session != "" {
		args = append(args, "--resume", req.Session)
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	// --allowedTools takes several values, so it goes last.
	args = append(args, "--allowedTools")
	for _, c := range readOnlyGit {
		args = append(args, "Bash(git "+c+")", "Bash(git "+c+" *)")
	}
	args = append(args, req.Allow...)

	var in strings.Builder
	if req.Context != "" {
		in.WriteString(req.Context)
		in.WriteString("\n\n")
	}
	in.WriteString("## Request\n\n")
	in.WriteString(req.Message)
	return args, in.String()
}

// ClaudeRule is the permission rule that lets Claude run exactly command.
func ClaudeRule(command string) string { return "Bash(" + command + ")" }

type claudeParser struct {
	session string
	result  *claudeResult
	sawText bool
}

type claudeLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Session string `json:"session_id"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	claudeResult
}

type claudeResult struct {
	IsError bool     `json:"is_error"`
	Result  string   `json:"result"`
	Errors  []string `json:"errors"`
	Cost    float64  `json:"total_cost_usd"`
	Denials []struct {
		Tool  string          `json:"tool_name"`
		Input json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
}

func (p *claudeParser) line(b []byte) []Event {
	var l claudeLine
	if json.Unmarshal(b, &l) != nil {
		return nil
	}
	sub := l.ParentToolUseID != nil && *l.ParentToolUseID != ""
	switch l.Type {
	case "system":
		if l.Subtype == "init" && l.Session != "" {
			p.session = l.Session
			return []Event{{Kind: EvSession, Session: l.Session}}
		}
	case "stream_event":
		if !sub && l.Event.Type == "content_block_delta" && l.Event.Delta.Type == "text_delta" {
			return []Event{{Kind: EvDelta, Text: l.Event.Delta.Text}}
		}
	case "assistant":
		if sub {
			return nil // a subagent's own steps
		}
		var evs []Event
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					p.sawText = true
					evs = append(evs, Event{Kind: EvText, Text: c.Text})
				}
			case "tool_use":
				detail, path := toolDetail(c.Name, c.Input)
				evs = append(evs, Event{Kind: EvTool, Tool: c.Name, Detail: detail, Path: path})
			}
		}
		return evs
	case "result":
		if l.Session != "" {
			p.session = l.Session
		}
		r := l.claudeResult
		p.result = &r
		// The final answer is normally already shown as an assistant
		// message; only use the result text when nothing was.
		if !p.sawText && !r.IsError && strings.TrimSpace(r.Result) != "" {
			return []Event{{Kind: EvText, Text: r.Result}}
		}
	}
	return nil
}

func (p *claudeParser) done(exitErr error, stderr string) Event {
	ev := Event{Kind: EvDone, Session: p.session}
	r := p.result
	if r == nil {
		ev.Err = processErr("Claude Code", exitErr, stderr)
		return ev
	}
	if r.Cost > 0 {
		ev.Cost = fmt.Sprintf("$%.2f", r.Cost)
	}
	for _, d := range r.Denials {
		detail, path := toolDetail(d.Tool, d.Input)
		ev.Denied = append(ev.Denied, Denial{Tool: d.Tool, Detail: detail, Path: path})
	}
	if r.IsError {
		msg := strings.Join(r.Errors, "; ")
		if msg == "" {
			msg = r.Result
		}
		if msg == "" {
			msg = "Claude Code reported an error"
		}
		ev.Err = errors.New(msg)
	}
	return ev
}

// toolDetail picks the human-readable argument of a tool call, and the
// file it changes for edit tools.
func toolDetail(tool string, input json.RawMessage) (detail, path string) {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	switch tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		path = str("file_path")
		if path == "" {
			path = str("notebook_path")
		}
		return path, path
	case "Bash", "PowerShell":
		return str("command"), ""
	}
	for _, k := range []string{"file_path", "path", "pattern", "url", "query", "description"} {
		if s := str(k); s != "" {
			return s, ""
		}
	}
	return "", ""
}

// processErr explains an agent that exited without a usable result.
func processErr(name string, exitErr error, stderr string) error {
	if line := lastLine(stderr); line != "" {
		return fmt.Errorf("%s: %s", name, line)
	}
	if exitErr != nil {
		return fmt.Errorf("%s: %w", name, exitErr)
	}
	return fmt.Errorf("%s ended without a reply", name)
}
