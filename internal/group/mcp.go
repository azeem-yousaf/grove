package group

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/azeem-yousaf/grove/internal/mcp"
)

// Tools are the names of the tools Serve offers, as agents' permission
// rules need them.
var Tools = server(Log{}, "", "").Names()

// maxWait caps how long the read tool waits for a message.
const maxWait = 5 * time.Minute

// Serve speaks the Model Context Protocol on in and out, offering member me
// the group's tools: members, post and read. It returns when in ends.
func Serve(in io.Reader, out io.Writer, log Log, me, version string) error {
	return server(log, me, version).Serve(in, out)
}

func server(log Log, me, version string) mcp.Server {
	tool := func(name, desc string, schema map[string]any) mcp.Tool {
		return mcp.Tool{Name: name, Description: desc, Schema: schema, Call: func(args json.RawMessage) (string, bool) {
			return call(log, me, name, args)
		}}
	}
	return mcp.Server{
		Name:         "grove",
		Version:      version,
		Instructions: "Your group chat in grove: talk to the other agents working on the same feature, and to the user.",
		Tools: []mcp.Tool{
			tool("members", "List the members of your group chat: each agent's task, branch name and worktree folder (which you may read but not change).",
				mcp.Object(map[string]any{})),
			tool("post", "Send a message to your group chat, or to one member with `to`. Use it to tell groupmates about anything they depend on "+
				"(APIs, types, file names, decisions) or to ask them something. Keep it short and specific. The user reads the group chat too. "+
				"Set `to` to \"you\" to address the user, or to \"planner\" for a decision from the planner when one watches the plan (see members).",
				mcp.Object(map[string]any{
					"text": map[string]any{"type": "string", "description": "The message."},
					"to":   map[string]any{"type": "string", "description": "A member's name (their branch) to address only them, \"you\" for the user, or \"planner\" for the planner. Leave out to address everyone."},
				}, "text")),
			tool("read", "Get the messages sent to you since you last read. With wait_seconds, waits up to that long (at most 300) "+
				"for a message when there is none yet, for example after asking a groupmate something.",
				mcp.Object(map[string]any{
					"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 300},
				})),
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
		if info.Planner != "" {
			fmt.Fprintf(&b, "\nThe planner watches this plan for the user and posts as %s; post with to %q when you need a decision from it.\n", info.Planner, Planner)
		}
		fmt.Fprintf(&b, "The user posts as %q; post with to %q to address them.\n", User, User)
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
	if name == User || IsPlanner(name) && info.Planner != "" {
		return true
	}
	for _, m := range info.Members {
		if m.Name == name {
			return true
		}
	}
	return false
}
