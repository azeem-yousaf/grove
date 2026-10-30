// Package control lets the planner run a started plan: grove writes what
// the plan's tasks and agents are doing to state.json, and the planner's
// tool server (`grove mcp-plan`) reads it and queues orders, such as a
// message for a task's agent or an answer to its approval request, in
// orders.jsonl, which grove takes and carries out, recording what came of
// each in results.jsonl for the tool to report. The files live in grove's
// store, so nothing has to stay running between the two programs.
package control

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	ID      string `json:"id"`   // stable for as long as it waits; answers name it
	Tool    string `json:"tool"` // "Bash" or "PowerShell"
	Command string `json:"command"`
	Reason  string `json:"reason,omitempty"` // why only the user may answer it, or what it does
	Planner bool   `json:"planner"`          // the planner may answer it; otherwise only the user
	Stale   bool   `json:"stale,omitempty"`  // the worktree changed since it was asked
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
	ID     string    `json:"id"` // grove records the outcome under it in results.jsonl
	Time   time.Time `json:"time"`
	Action string    `json:"action"`
	Task   string    `json:"task,omitempty"` // its branch
	Plan   string    `json:"plan,omitempty"` // its ID, for orders about a whole plan
	Item   string    `json:"item,omitempty"` // the approval's ID, for Approve and Deny
	Group  string    `json:"group,omitempty"`
	To     string    `json:"to,omitempty"`
	Text   string    `json:"text,omitempty"`
	Note   string    `json:"note,omitempty"`
	Prefix bool      `json:"prefix,omitempty"` // for Approve: the command's family, not just the line
	By     string    `json:"by,omitempty"`     // the planner that sent it (Caller.String); grove refuses orders without one
}

// Caller is the planner a tool server works for: the lead planner (the
// zero Caller), or the planner of one group of a plan, which sees and acts
// on that group only.
type Caller struct{ Plan, Group string }

// Lead is the lead planner's Caller.String.
const Lead = "lead"

// String is how --as and Order.By name the caller.
func (c Caller) String() string {
	if c.Group == "" {
		return Lead
	}
	return "crew:" + c.Plan + ":" + c.Group
}

// ParseCaller reads what Caller.String wrote. Plan IDs have no colon, so
// the group is everything after the second one.
func ParseCaller(s string) (Caller, bool) {
	if s == Lead {
		return Caller{}, true
	}
	rest, ok := strings.CutPrefix(s, "crew:")
	if !ok {
		return Caller{}, false
	}
	planID, groupID, ok := strings.Cut(rest, ":")
	if !ok || planID == "" || groupID == "" {
		return Caller{}, false
	}
	return Caller{planID, groupID}, true
}

// For is what c may see of s: everything for the lead; for a group's
// planner, its plan with only its group's tasks and chat.
func (s State) For(c Caller) State {
	if c.Group == "" {
		return s
	}
	var out State
	for _, p := range s.Plans {
		if p.ID != c.Plan {
			continue
		}
		var tasks []Task
		for _, t := range p.Tasks {
			if t.Group == c.Group {
				tasks = append(tasks, t)
			}
		}
		groups := p.Groups
		p.Tasks, p.Groups = tasks, nil
		if slices.Contains(groups, c.Group) {
			p.Groups = []string{c.Group}
		}
		out.Plans = append(out.Plans, p)
	}
	return out
}

// Outcomes of an order, or of a decision in a grove-orchestrate block.
const (
	Applied        = "applied"
	AlreadyApplied = "already applied" // the same answer was given before; nothing changed
	NotFound       = "not found"
	NeedsUser      = "needs the user" // only the user may answer it; it waits for them
	Failed         = "failed"
)

// Result is what came of an order.
type Result struct {
	Order   string    `json:"order"`
	Time    time.Time `json:"time"`
	Outcome string    `json:"outcome"`
	Detail  string    `json:"detail,omitempty"`
}

// keepResults is how many results results.jsonl keeps.
const keepResults = 200

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

// NewID makes a random order ID.
func NewID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "o-" + hex.EncodeToString(b)
}

// Queue appends an order for grove, giving it an ID and time when it has
// none.
func Queue(dir string, o Order) error {
	if o.ID == "" {
		o.ID = NewID()
	}
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

func resultsFile(dir string) string { return filepath.Join(dir, "results.jsonl") }

// Record adds what came of orders to results.jsonl, keeping the latest
// keepResults.
func Record(dir string, rs []Result) error {
	if len(rs) == 0 {
		return nil
	}
	unlock, err := lock(dir)
	if err != nil {
		return err
	}
	defer unlock()
	all := append(readResults(dir), rs...)
	if len(all) > keepResults {
		all = all[len(all)-keepResults:]
	}
	var b bytes.Buffer
	for _, r := range all {
		if r.Time.IsZero() {
			r.Time = time.Now()
		}
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		b.Write(append(line, '\n'))
	}
	// Replaced whole, so a tool reading it never sees half a line.
	tmp := filepath.Join(dir, ".results.tmp")
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, resultsFile(dir))
}

func readResults(dir string) []Result {
	b, err := os.ReadFile(resultsFile(dir))
	if err != nil {
		return nil
	}
	var out []Result
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var r Result
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Order != "" {
			out = append(out, r)
		}
	}
	return out
}

// ResultOf finds what came of the order with ID id, once grove has carried
// it out.
func ResultOf(dir, id string) (Result, bool) {
	rs := readResults(dir)
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Order == id {
			return rs[i], true
		}
	}
	return Result{}, false
}

// Activity is one answer to an approval request, as approvals.jsonl in
// grove's store records it.
type Activity struct {
	Time    time.Time `json:"time"`
	ID      string    `json:"id"` // the approval's
	Plan    string    `json:"plan,omitempty"`
	Task    string    `json:"task,omitempty"` // its branch
	Command string    `json:"command"`
	Verdict string    `json:"verdict"` // "approve" or "deny"
	By      string    `json:"by"`      // "planner", "user" or "grove-policy"
	Outcome string    `json:"outcome"`
	Reason  string    `json:"reason,omitempty"`
}

// LogActivity appends to approvals.jsonl in grove's store.
func LogActivity(storeDir string, as []Activity) error {
	if len(as) == 0 {
		return nil
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return err
	}
	var b bytes.Buffer
	for _, a := range as {
		if a.Time.IsZero() {
			a.Time = time.Now()
		}
		line, err := json.Marshal(a)
		if err != nil {
			return err
		}
		b.Write(append(line, '\n'))
	}
	f, err := os.OpenFile(filepath.Join(storeDir, "approvals.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// NormCommand is a command as answers compare it: runs of white space are
// one space, typographic quotes are plain ones, and a backslash before a
// quote or backtick is dropped, since it is easily lost or added when a
// command is quoted back.
func NormCommand(cmd string) string {
	cmd = strings.NewReplacer("‘", "'", "’", "'", "“", `"`, "”", `"`,
		"\\`", "`", `\"`, `"`, `\'`, "'").Replace(cmd)
	return strings.Join(strings.Fields(cmd), " ")
}

// FindApproval finds the approval a task waits on by its ID, else by its
// command.
func (t Task) FindApproval(id, command string) (Approval, bool) {
	for _, a := range t.Approvals {
		if id != "" && a.ID == id {
			return a, true
		}
	}
	if id != "" {
		return Approval{}, false
	}
	want := NormCommand(command)
	for _, a := range t.Approvals {
		if want != "" && NormCommand(a.Command) == want {
			return a, true
		}
	}
	return Approval{}, false
}

// FindApproval finds the task waiting on the approval with ID id.
func (s State) FindApproval(id string) (Task, bool) {
	for _, p := range s.Plans {
		for _, t := range p.Tasks {
			if _, ok := t.FindApproval(id, ""); ok {
				return t, true
			}
		}
	}
	return Task{}, false
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
