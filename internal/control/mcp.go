package control

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/azeem-yousaf/grove/internal/mcp"
)

// Tools are the names of the tools Serve offers, as the planner's
// permission rules need them.
var Tools = server("", "").Names()

// Serve speaks the Model Context Protocol on in and out, offering the
// planner the tools to run the started plans whose state is in dir. It
// returns when in ends.
func Serve(in io.Reader, out io.Writer, dir, version string) error {
	return server(dir, version).Serve(in, out)
}

func server(dir, version string) mcp.Server {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	task := str("The task's branch, as the tasks tool lists it.")
	return mcp.Server{
		Name:         "grove",
		Version:      version,
		Instructions: "Run the plans the user started in grove: see what each task's agent is doing, message it, answer its approval requests and mark tasks done.",
		Tools: []mcp.Tool{
			{
				Name:        "tasks",
				Description: "List the started plans' tasks: status, worktree, whether the agent is working, commands it waits for approval to run, and the end of its last reply.",
				Schema:      mcp.Object(map[string]any{}),
				Call:        func(json.RawMessage) (string, bool) { return tasks(dir) },
			},
			{
				Name: "message",
				Description: "Send a task's agent a message, which starts a turn for it: answer its question, correct it, or tell it what to do next. " +
					"If it is working, the message waits until its turn ends. Messaging a finished or failed task reopens it.",
				Schema: mcp.Object(map[string]any{"task": task, "text": str("The message.")}, "task", "text"),
				Call: func(args json.RawMessage) (string, bool) {
					return order(dir, Message, args)
				},
			},
			{
				Name:        "approve",
				Description: "Let a task's agent run a command it is waiting on (see tasks). It may run that exact command from then on without asking.",
				Schema:      mcp.Object(map[string]any{"task": task, "command": str("The command, exactly as tasks shows it.")}, "task", "command"),
				Call: func(args json.RawMessage) (string, bool) {
					return order(dir, Approve, args)
				},
			},
			{
				Name:        "deny",
				Description: "Refuse a command a task's agent is waiting on. The reason is passed on to the agent.",
				Schema:      mcp.Object(map[string]any{"task": task, "command": str("The command, exactly as tasks shows it."), "reason": str("Why, and what to do instead.")}, "task", "command"),
				Call: func(args json.RawMessage) (string, bool) {
					return order(dir, Deny, args)
				},
			},
			{
				Name:        "mark",
				Description: "Mark a task finished (its work is done and checked) or failed (it can't be done as planned).",
				Schema: mcp.Object(map[string]any{
					"task":   task,
					"status": map[string]any{"type": "string", "enum": []string{"finished", "failed"}},
					"note":   str("A sentence on why, shown to the user."),
				}, "task", "status"),
				Call: func(args json.RawMessage) (string, bool) {
					return order(dir, "mark", args)
				},
			},
		},
	}
}

func tasks(dir string) (string, bool) {
	st, err := ReadState(dir)
	if err != nil {
		return "Couldn't read the plans: " + err.Error(), true
	}
	if len(st.Plans) == 0 {
		return "No plan is running.", false
	}
	var b strings.Builder
	for _, p := range st.Plans {
		fmt.Fprintf(&b, "Plan %q (%s)\n", p.Title, p.Status)
		for _, t := range p.Tasks {
			fmt.Fprintf(&b, "\n- %s: %s · %s", t.Branch, t.Title, t.Status)
			if t.Busy {
				b.WriteString(" · agent working")
			}
			if t.Group != "" {
				fmt.Fprintf(&b, " · group %s", t.Group)
			}
			if t.Path != "" {
				fmt.Fprintf(&b, " · worktree %s", t.Path)
			}
			b.WriteString("\n")
			if t.Error != "" {
				fmt.Fprintf(&b, "  Error: %s\n", t.Error)
			}
			for _, a := range t.Approvals {
				fmt.Fprintf(&b, "  Waiting for approval (%s): %s\n", a.Tool, a.Command)
			}
			if t.LastReply != "" {
				fmt.Fprintf(&b, "  Last reply:\n%s\n", indent(t.LastReply, "    "))
			}
		}
		b.WriteString("\n")
	}
	return b.String(), false
}

func indent(s, by string) string {
	return by + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n"+by)
}

// order checks an order against the tasks grove last described and
// queues it.
func order(dir, action string, args json.RawMessage) (string, bool) {
	var a struct{ Task, Text, Command, Reason, Status, Note string }
	if err := json.Unmarshal(args, &a); err != nil {
		return "Invalid arguments: " + err.Error(), true
	}
	st, err := ReadState(dir)
	if err != nil {
		return "Couldn't read the plans: " + err.Error(), true
	}
	t, ok := st.Find(a.Task)
	if !ok {
		return fmt.Sprintf("No running task has the branch %q; use the tasks tool to see them.", a.Task), true
	}
	o := Order{Action: action, Task: t.Branch}
	done := ""
	switch action {
	case Message:
		if strings.TrimSpace(a.Text) == "" {
			return "The message is empty.", true
		}
		if t.Path == "" {
			return t.Branch + " hasn't started yet: its worktree is still to be made.", true
		}
		o.Text, done = a.Text, "Sent. It reaches the agent as soon as it isn't working; you'll hear when its turn ends."
	case Approve, Deny:
		cmd := strings.TrimSpace(a.Command)
		found := false
		for _, ap := range t.Approvals {
			found = found || ap.Command == cmd
		}
		if !found {
			return fmt.Sprintf("%s isn't waiting to run %q; use the tasks tool to see what it waits on.", t.Branch, cmd), true
		}
		o.Text, o.Note = cmd, a.Reason
		done = "Approved; the agent carries on once every command it waits on has an answer."
		if action == Deny {
			done = "Denied; the agent is told why once every command it waits on has an answer."
		}
	case "mark":
		switch a.Status {
		case "finished":
			o.Action = Finish
		case "failed":
			o.Action = Fail
		default:
			return `status must be "finished" or "failed".`, true
		}
		o.Note, done = a.Note, "Marked "+a.Status+"."
	}
	if err := Queue(dir, o); err != nil {
		return "Couldn't reach grove: " + err.Error(), true
	}
	return done, false
}
