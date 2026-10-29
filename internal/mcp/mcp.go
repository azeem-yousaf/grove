// Package mcp is a minimal Model Context Protocol server over stdio: one
// JSON-RPC message per line, offering tools and nothing else. grove's tool
// servers for agents (the group chat, the planner's control of a plan) are
// built on it.
package mcp

import (
	"bufio"
	"encoding/json"
	"io"
)

// Tool is one tool a server offers.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any // its arguments, as a JSON schema
	// Call runs the tool, returning its text and whether it failed.
	Call func(args json.RawMessage) (string, bool)
}

// Server describes a tool server.
type Server struct {
	Name         string
	Version      string
	Instructions string
	Tools        []Tool
}

// Object is the JSON schema of an object with props, of which required
// must be given.
func Object(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// rpc is one JSON-RPC message from the client.
type rpc struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve speaks the protocol on in and out, returning when in ends.
func (s Server) Serve(in io.Reader, out io.Writer) error {
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
				"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
				"instructions":    s.Instructions,
			}, nil)
		case "ping":
			err = reply(req.ID, map[string]any{}, nil)
		case "tools/list":
			list := []map[string]any{}
			for _, t := range s.Tools {
				list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.Schema})
			}
			err = reply(req.ID, map[string]any{"tools": list}, nil)
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if jerr := json.Unmarshal(req.Params, &p); jerr != nil {
				err = reply(req.ID, nil, &rpcError{-32602, "invalid params"})
				break
			}
			text, failed := "No tool called "+p.Name+".", true
			for _, t := range s.Tools {
				if t.Name == p.Name {
					text, failed = t.Call(p.Arguments)
				}
			}
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

// Names lists the tools' names, as agents' permission rules need them.
func (s Server) Names() []string {
	var out []string
	for _, t := range s.Tools {
		out = append(out, t.Name)
	}
	return out
}
