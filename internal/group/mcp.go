package group

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Tools are the names of the tools Serve offers, as agents' permission
// rules need them.
var Tools = []string{"members", "post", "read"}

// maxWait caps how long the read tool waits for a message.
const maxWait = 5 * time.Minute

// rpc is one JSON-RPC message from the client.
type rpc struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Serve speaks the Model Context Protocol on in and out, one JSON-RPC
// message per line, offering member me the group's tools: members, post
// and read. It returns when in ends.
func Serve(in io.Reader, out io.Writer, log Log, me, version string) error {
	enc := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any, rerr *rpcError) error {
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rerr != nil {
			msg["error"] = rerr
		} else {
			msg["result"] = result
		}
		return enc.Encode(msg)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var req rpc
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			if err := reply(json.RawMessage("null"), nil, &rpcError{-32700, "parse error"}); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification, such as notifications/initialized
		}
		var err error
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			err = reply(req.ID, map[string]any{
				"protocolVersion": p.ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "grove", "version": version},
				"instructions":    "Your group chat in grove: talk to the other agents working on the same feature, and to the user.",
			}, nil)
		case "ping":
			err = reply(req.ID, map[string]any{}, nil)
		case "tools/list":
			err = reply(req.ID, map[string]any{"tools": toolList()}, nil)
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if jerr := json.Unmarshal(req.Params, &p); jerr != nil {
				err = reply(req.ID, nil, &rpcError{-32602, "invalid params"})
				break
			}
			text, failed := call(log, me, p.Name, p.Arguments)
			err = reply(req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": failed,
			}, nil)
		default:
			err = reply(req.ID, nil, &rpcError{-32601, "method not found: " + req.Method})
		}
		if err != nil {
			return err
		}
	}
	return sc.Err()
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func toolList() []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	return []map[string]any{
		{
			"name":        "members",
			"description": "List the members of your group chat: each agent's task, branch name and worktree folder (which you may read but not change).",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name": "post",
			"description": "Send a message to your group chat, or to one member with `to`. Use it to tell groupmates about anything they depend on " +
				"(APIs, types, file names, decisions) or to ask them something. Keep it short and specific. The user reads the group chat too.",
			"inputSchema": obj(map[string]any{
				"text": map[string]any{"type": "string", "description": "The message."},
				"to":   map[string]any{"type": "string", "description": "A member's name (their branch) to address only them. Leave out to address everyone."},
			}, "text"),
		},
		{
			"name": "read",
			"description": "Get the messages sent to you since you last read. With wait_seconds, waits up to that long (at most 300) " +
				"for a message when there is none yet, for example after asking a groupmate something.",
			"inputSchema": obj(map[string]any{
				"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 300},
			}),
		},
	}
}

// call runs one tool, returning its text and whether it failed.
func call(log Log, me, name string, args json.RawMessage) (string, bool) {
	switch name {
	case "members":
		info, err := log.ReadInfo()
		if err != nil {
			return "Couldn't read the group: " + err.Error(), true
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Group %q of plan %q. You are %s.\n\n", info.Name, info.Plan, me)
		for _, m := range info.Members {
			you := ""
			if m.Name == me {
				you = " (you)"
			}
			fmt.Fprintf(&b, "- %s%s: %s", m.Name, you, m.Task)
			if m.Path != "" {
				fmt.Fprintf(&b, " · worktree %s", m.Path)
			} else {
				b.WriteString(" · not started yet")
			}
			if m.Status != "" {
				fmt.Fprintf(&b, " · %s", m.Status)
			}
			b.WriteString("\n")
		}
		return b.String(), false
	case "post":
		var a struct{ Text, To string }
		if err := json.Unmarshal(args, &a); err != nil {
			return "Invalid arguments: " + err.Error(), true
		}
		if a.To != "" {
			if info, err := log.ReadInfo(); err == nil && !hasMember(info, a.To) {
				return fmt.Sprintf("Nobody called %q is in this group; use the members tool to see who is.", a.To), true
			}
		}
		if err := log.Post(Message{From: me, To: a.To, Text: a.Text}); err != nil {
			return "Couldn't post: " + err.Error(), true
		}
		return "Posted.", false
	case "read":
		var a struct {
			Wait int `json:"wait_seconds"`
		}
		_ = json.Unmarshal(args, &a)
		wait := min(time.Duration(max(a.Wait, 0))*time.Second, maxWait)
		msgs, err := log.Read(me, wait)
		if err != nil {
			return "Couldn't read: " + err.Error(), true
		}
		if len(msgs) == 0 {
			return "No new messages.", false
		}
		return Format(msgs), false
	}
	return "No tool called " + name + ".", true
}

func hasMember(info Info, name string) bool {
	if name == User {
		return true
	}
	for _, m := range info.Members {
		if m.Name == name {
			return true
		}
	}
	return false
}
