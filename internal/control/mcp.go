package control

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/azeem-yousaf/grove/internal/mcp"
)

// Tools are the names of the tools Serve offers, as the planner's
// permission rules need them.
var Tools = server("", "", Caller{}).Names()

// Serve speaks the Model Context Protocol on in and out, offering planner
// as the tools to run the started plans whose state is in dir. It returns
// when in ends.
func Serve(in io.Reader, out io.Writer, dir, version string, as Caller) error {
	return server(dir, version, as).Serve(in, out)
}

func server(dir, version string, as Caller) mcp.Server {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	task := str("The task's branch, as the tasks tool lists it.")
	id := str("The approval's id, as tasks shows it. Preferred over command.")
	command := str("The command, as tasks shows it; used when there is no id.")
	return mcp.Server{
		Name:    "grove",
		Version: version,
		Instructions: "Run the plans the user started in grove: see what each task's agent is doing, message it, answer its approval requests, mark tasks done, " +
			"merge a finished plan for the user to try and talk to them in its plan chat.",
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
					return order(dir, as, Message, args)
				},
			},
			{
				Name: "approve",
				Description: "Let a task's agent run a command it is waiting on (see tasks). It may run that exact command from then on without asking. " +
					"Only commands tasks marks as yours to answer; the rest wait for the user. You get grove's answer: applied, already applied, not found or needs the user.",
				Schema: mcp.Object(map[string]any{"task": task, "id": id, "command": command, "prefix": map[string]any{
					"type": "boolean",
					"description": "Allow the command's whole family (program and subcommand with any arguments, e.g. every `go test`) instead of the exact line. " +
						"Not for git, interpreters or more than one command; those get the exact line.",
				}}),
				Call: func(args json.RawMessage) (string, bool) {
					return order(dir, as, Approve, args)
				},
			},
			{
				Name:        "deny",
				Description: "Refuse a command a task's agent is waiting on. The reason is passed on to the agent. You get grove's answer.",
				Schema:      mcp.Object(map[string]any{"task": task, "id": id, "command": command, "reason": str("Why, and what to do instead.")}),
				Call: func(args json.RawMessage) (string, bool) {
					return order(dir, as, Deny, args)
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
					return order(dir, as, "mark", args)
				},
			},
			{
				Name: "test",
				Description: "Run the plan's test command, or the command you give, in a task's worktree in the background, and return at once. " +
					"When it ends grove sends you a new request with whether it passed and the end of its output; carry on with other things meanwhile. " +
					"Use it for anything long (test suites, builds) instead of running it yourself. One run per worktree at a time; git commands aren't allowed.",
				Schema: mcp.Object(map[string]any{
					"task":    task,
					"command": str("The command to run, through sh (cmd.exe on Windows). Leave out for the plan's test command."),
				}, "task"),
				Call: func(args json.RawMessage) (string, bool) {
					return order(dir, as, Test, args)
				},
			},
			{
				Name: "post",
				Description: "Post in a plan's chat: the plan chat, where the user gives you feedback on the plan and you answer them, " +
					"or one of its group chats. Address one task's agent with `to`, or leave it out for everyone.",
				Schema: mcp.Object(map[string]any{
					"plan":  str("The plan's ID, as tasks shows it. Optional when only one plan is running."),
					"group": str(`A group ID from tasks, or leave out for the plan chat.`),
					"to":    str("A task's branch, to address only its agent."),
					"text":  str("The message."),
				}, "text"),
				Call: func(args json.RawMessage) (string, bool) { return post(dir, as, args) },
			},
			{
				Name: "merge",
				Description: "Merge a plan whose tasks are all finished or failed: grove commits each finished task's worktree and merges their branches, " +
					"in plan order, into the plan's own worktree (made the first time) for the user to try. Merging again brings in later changes. " +
					"If a branch conflicts with the work merged so far, grove asks that task's agent to resolve it and merges again once it is done. You hear how it went.",
				Schema: mcp.Object(map[string]any{"plan": str("The plan's ID, as tasks shows it. Optional when only one plan is running.")}),
				Call:   func(args json.RawMessage) (string, bool) { return planOrder(dir, as, Merge, args) },
			},
			{
				Name:        "close",
				Description: "Close a merged plan once the user is happy with it. Its worktrees stay for the user to ship.",
				Schema: mcp.Object(map[string]any{
					"plan": str("The plan's ID, as tasks shows it. Optional when only one plan is running."),
					"note": str("A sentence on what the user accepted."),
				}),
				Call: func(args json.RawMessage) (string, bool) { return planOrder(dir, as, Close, args) },
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
		fmt.Fprintf(&b, "Plan %q (id %s, %s)\n", p.Title, p.ID, p.Status)
		if len(p.Groups) > 0 {
			fmt.Fprintf(&b, "Group chats: %s\n", strings.Join(p.Groups, ", "))
		}
		if p.Merge != "" {
			fmt.Fprintf(&b, "Merge: %s\n", p.Merge)
		}
		if p.Test != "" {
			fmt.Fprintf(&b, "Test command: %s\n", p.Test)
		}
		for _, t := range p.Tasks {
			fmt.Fprintf(&b, "\n- %s: %s · %s", t.Branch, t.Title, t.Status)
			if t.Busy {
				b.WriteString(" · agent working")
			}
			if t.Testing != "" {
				fmt.Fprintf(&b, " · grove is running `%s`", t.Testing)
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
				who := "yours to answer"
				if !a.Planner {
					who = "only the user may answer"
				}
				if a.Reason != "" {
					who += ": " + a.Reason
				}
				stale := ""
				if a.Stale {
					stale = " (worktree changed since it was asked)"
				}
				fmt.Fprintf(&b, "  Waiting for approval, id %s (%s, %s): %s%s\n", a.ID, a.Tool, who, a.Command, stale)
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

// Wait is how long approve, deny and message wait for grove to carry the
// order out.
var Wait = 5 * time.Second

// order checks an order against the tasks grove last described and
// queues it.
func order(dir string, as Caller, action string, args json.RawMessage) (string, bool) {
	var a struct {
		Task, ID, Text, Command, Reason, Status, Note string
		Prefix                                        bool
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "Invalid arguments: " + err.Error(), true
	}
	st, err := ReadState(dir)
	if err != nil {
		return "Couldn't read the plans: " + err.Error(), true
	}
	if a.Task == "" && a.ID != "" {
		if t, ok := st.FindApproval(a.ID); ok {
			a.Task = t.Branch
		}
	}
	p, t, ok := st.FindTask(a.Task)
	if !ok {
		return fmt.Sprintf("No running task has the branch %q; use the tasks tool to see them.", a.Task), true
	}
	if as.Group != "" && (p.ID != as.Plan || t.Group != as.Group) {
		return fmt.Sprintf("%s isn't a task of your group; its planner handles it.", t.Branch), true
	}
	o := Order{ID: NewID(), Action: action, Task: t.Branch, By: as}
	done := ""
	switch action {
	case Test:
		if t.Path == "" {
			return t.Branch + " hasn't started yet: its worktree is still to be made.", true
		}
		if t.Testing != "" {
			return fmt.Sprintf("grove is already running `%s` in %s's worktree; wait for its result.", t.Testing, t.Branch), true
		}
		if strings.TrimSpace(a.Command) == "" && p.Test == "" {
			return fmt.Sprintf("Plan %q has no test command: give the command to run.", p.Title), true
		}
		// grove checks the command isn't git and has the last word.
		o.Text = strings.TrimSpace(a.Command)
	case Message:
		if strings.TrimSpace(a.Text) == "" {
			return "The message is empty.", true
		}
		if t.Path == "" {
			return t.Branch + " hasn't started yet: its worktree is still to be made.", true
		}
		o.Text = a.Text
	case Approve, Deny:
		if a.ID == "" && strings.TrimSpace(a.Command) == "" {
			return "Say which command: give its id (or the command).", true
		}
		// grove has the last word: the approval may have been answered
		// since state.json was written, which grove reports as already
		// applied.
		ap, found := t.FindApproval(a.ID, a.Command)
		if found {
			o.Item, o.Text = ap.ID, ap.Command
		} else {
			o.Item, o.Text = a.ID, strings.TrimSpace(a.Command)
		}
		o.Note, o.Prefix = a.Reason, action == Approve && a.Prefix
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
	if done != "" {
		return done, false
	}
	return awaitResult(dir, o.ID)
}

// awaitResult waits up to Wait for what came of an order and reports it.
func awaitResult(dir, id string) (string, bool) {
	deadline := time.Now().Add(Wait)
	for {
		if r, ok := ResultOf(dir, id); ok {
			text := r.Outcome
			if r.Detail != "" {
				text += ": " + r.Detail
			}
			text = strings.ToUpper(text[:1]) + text[1:]
			return text, r.Outcome != Applied && r.Outcome != AlreadyApplied
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("Still pending: grove hasn't carried out order %s yet (it may be busy or closed). Check with tasks before relying on it.", id), false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// post checks a chat message against the plans grove last described and
// queues it.
func post(dir string, as Caller, args json.RawMessage) (string, bool) {
	var a struct{ Plan, Group, To, Text string }
	if err := json.Unmarshal(args, &a); err != nil {
		return "Invalid arguments: " + err.Error(), true
	}
	if strings.TrimSpace(a.Text) == "" {
		return "The message is empty.", true
	}
	p, msg := findPlan(dir, a.Plan)
	if msg != "" {
		return msg, true
	}
	if a.Group != "" && a.Group != "plan" && !slices.Contains(p.Groups, a.Group) {
		return fmt.Sprintf("Plan %q has no group %q; its groups are: %s.", p.Title, a.Group, strings.Join(p.Groups, ", ")), true
	}
	if a.To != "" && !slices.ContainsFunc(p.Tasks, func(t Task) bool { return strings.EqualFold(t.Branch, a.To) }) {
		return fmt.Sprintf("Plan %q has no task with the branch %q.", p.Title, a.To), true
	}
	if err := Queue(dir, Order{Action: Post, Plan: p.ID, Group: a.Group, To: a.To, Text: a.Text, By: as}); err != nil {
		return "Couldn't reach grove: " + err.Error(), true
	}
	return "Posted.", false
}

// findPlan is the plan named by ID or title, or the only one when name is
// empty, or a message saying why there is none.
func findPlan(dir, name string) (Plan, string) {
	st, err := ReadState(dir)
	if err != nil {
		return Plan{}, "Couldn't read the plans: " + err.Error()
	}
	if name == "" {
		if len(st.Plans) != 1 {
			return Plan{}, fmt.Sprintf("%d plans are running: say which with plan.", len(st.Plans))
		}
		return st.Plans[0], ""
	}
	if p, ok := st.FindPlan(name); ok {
		return p, ""
	}
	return Plan{}, fmt.Sprintf("No running plan is called %q; use the tasks tool to see them.", name)
}

// planOrder checks an order about a whole plan and queues it.
func planOrder(dir string, as Caller, action string, args json.RawMessage) (string, bool) {
	var a struct{ Plan, Note string }
	if err := json.Unmarshal(args, &a); err != nil {
		return "Invalid arguments: " + err.Error(), true
	}
	p, msg := findPlan(dir, a.Plan)
	if msg != "" {
		return msg, true
	}
	done := "Merging; you'll hear how it went."
	switch action {
	case Merge:
		for _, t := range p.Tasks {
			if t.Status != "finished" && t.Status != "failed" {
				return fmt.Sprintf("%s is still %s: merge once every task is finished or failed.", t.Branch, t.Status), true
			}
		}
	case Close:
		if p.Status != "review" {
			return fmt.Sprintf("Plan %q isn't merged and waiting for the user (it is %s).", p.Title, p.Status), true
		}
		done = "Closed."
	}
	if err := Queue(dir, Order{Action: action, Plan: p.ID, Note: a.Note, By: as}); err != nil {
		return "Couldn't reach grove: " + err.Error(), true
	}
	return done, false
}
