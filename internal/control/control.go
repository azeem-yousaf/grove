// Package control lets the planner run a started plan: grove writes what
// the plan's tasks and agents are doing to state.json, and the planner's
// tool server (`grove mcp-plan`) reads it and queues orders, such as a
// message for a task's agent or an answer to its approval request, in
// orders.jsonl, which grove takes and carries out. Both files live in
// grove's store, so nothing has to stay running between the two programs.
package control

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/azeem-yousaf/grove/internal/fslock"
)

// State is what grove tells the planner about the started plans.
type State struct {
	Plans []Plan `json:"plans"`
}

// Plan is one started plan.
type Plan struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Groups []string `json:"groups,omitempty"` // its group chats' IDs
	Tasks  []Task   `json:"tasks"`
	Merge  string   `json:"merge,omitempty"` // what its merge worktree holds, once there is one
}

// Task is one task of a plan and what its agent is doing.
type Task struct {
	Branch    string     `json:"branch"` // unique, so it names the task
	Title     string     `json:"title"`
	Group     string     `json:"group,omitempty"`
	Status    string     `json:"status"`
	Error     string     `json:"error,omitempty"`
	Path      string     `json:"path,omitempty"` // its worktree, once made
	Busy      bool       `json:"busy,omitempty"` // its agent is in a turn
	LastReply string     `json:"last_reply,omitempty"`
	Approvals []Approval `json:"approvals,omitempty"` // commands it waits to run
}

// Approval is a command a task's agent was refused and waits on.
type Approval struct {
	Tool    string `json:"tool"` // "Bash" or "PowerShell"
	Command string `json:"command"`
}

// Order actions.
const (
	Message = "message" // Text goes to the task's agent as a turn
	Approve = "approve" // Text is the command to allow
	Deny    = "deny"    // Text is the command to refuse, Note why
	Finish  = "finish"  // mark the task finished, Note why
	Fail    = "fail"    // mark the task failed, Note why
	Post    = "post"    // Text goes to a chat of Plan: Group ("" for the plan chat), To one member or everyone
	Merge   = "merge"   // merge Plan's finished tasks into its own worktree
	Close   = "close"   // end Plan in review, Note why
)

// Order is something the planner asked grove to do.
type Order struct {
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Task   string    `json:"task,omitempty"` // its branch
	Plan   string    `json:"plan,omitempty"` // its ID, for orders about a whole plan
	Group  string    `json:"group,omitempty"`
	To     string    `json:"to,omitempty"`
	Text   string    `json:"text,omitempty"`
	Note   string    `json:"note,omitempty"`
}

// Dir is the folder under grove's store.
func Dir(storeDir string) string { return filepath.Join(storeDir, "control") }

func lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	unlock, err := fslock.Lock(filepath.Join(dir, "lock"))
	if errors.Is(err, fslock.ErrBusy) {
		return nil, errors.New("grove's plan control is locked by another program")
	}
	return unlock, err
}

// WriteState replaces state.json.
func WriteState(dir string, s State) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".state.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "state.json"))
}

// ReadState loads what WriteState saved.
func ReadState(dir string) (State, error) {
	var s State
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func ordersFile(dir string) string { return filepath.Join(dir, "orders.jsonl") }

// Queue appends an order for grove.
func Queue(dir string, o Order) error {
	if o.Time.IsZero() {
		o.Time = time.Now()
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(ordersFile(dir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Take returns the queued orders, oldest first, and empties the queue. A
// line cut short by a crash is skipped.
func Take(dir string) ([]Order, error) {
	if _, err := os.Stat(ordersFile(dir)); errors.Is(err, os.ErrNotExist) {
		return nil, nil // nothing queued; don't take the lock
	}
	unlock, err := lock(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, err := os.ReadFile(ordersFile(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Order
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var o Order
		if json.Unmarshal(sc.Bytes(), &o) == nil && o.Action != "" {
			out = append(out, o)
		}
	}
	if err := os.Remove(ordersFile(dir)); err != nil {
		return nil, err
	}
	return out, nil
}

// FindPlan looks a plan up by its ID or title.
func (s State) FindPlan(name string) (Plan, bool) {
	for _, p := range s.Plans {
		if p.ID == name || strings.EqualFold(p.Title, name) {
			return p, true
		}
	}
	return Plan{}, false
}

// Find looks a task up by its branch.
func (s State) Find(branch string) (Task, bool) {
	for _, p := range s.Plans {
		for _, t := range p.Tasks {
			if strings.EqualFold(t.Branch, branch) {
				return t, true
			}
		}
	}
	return Task{}, false
}
