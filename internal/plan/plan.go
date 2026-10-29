// Package plan holds the planner's work: a request split into tasks, each
// with its own branch and worktree, optionally gathered into groups whose
// agents may talk to each other. Plans are proposed by the planner agent,
// edited and confirmed by the user, and saved in the repository's store.
package plan

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/azeem-yousaf/grove/internal/store"
)

// Status is where a plan is in its life.
type Status string

const (
	Draft   Status = "draft"   // proposed, waiting for the user to confirm
	Running Status = "running" // confirmed: worktrees made, agents at work
	Review  Status = "review"  // merged by the planner, waiting for the user's feedback
	Done    Status = "done"    // every task finished; merging is up to the user
)

// TaskStatus is where one task is.
type TaskStatus string

const (
	Proposed TaskStatus = "proposed" // in a draft plan
	Queued   TaskStatus = "queued"   // confirmed, waiting for an agent slot
	Working  TaskStatus = "working"  // its agent is running or waiting on the user
	Finished TaskStatus = "finished" // its agent reported the task complete
	Failed   TaskStatus = "failed"
)

// Plan is one request turned into tasks.
type Plan struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Request string `json:"request"`          // what the user asked for, as typed
	Source  string `json:"source,omitempty"` // where it came from, e.g. "jira:SHOP-12"
	Base    string `json:"base,omitempty"`   // branch new worktrees start from
	Agent   string `json:"agent,omitempty"`  // "claude" or "codex", chosen when the plan starts
	// Test is the command that builds and tests the merged plan, as the
	// planner proposed it; [planner] test in the config wins over it.
	Test string `json:"test,omitempty"`
	// Orchestrated plans are watched by the planner, which approves their
	// agents' commands, answers their questions and follows their group
	// chats; otherwise all of that waits for the user.
	Orchestrated bool      `json:"orchestrated,omitempty"`
	Status       Status    `json:"status"`
	Created      time.Time `json:"created"`
	Updated      time.Time `json:"updated"`
	Groups       []Group   `json:"groups,omitempty"`
	Tasks        []Task    `json:"tasks"`
	Merge        *Merge    `json:"merge,omitempty"` // where the planner merges the finished tasks
}

// Merge is the plan's own worktree, where the planner merges the finished
// tasks' branches for the user to try.
type Merge struct {
	Branch string `json:"branch"`
	Path   string `json:"path,omitempty"`
	// Fixing is the branch of a task whose agent is resolving a conflict
	// with the work merged so far; the merge goes on once it is done.
	Fixing string `json:"fixing,omitempty"`
	// Tests is how the test command went on the latest merge: TestsPassed,
	// TestsFailed, or "" before they ran or without a test command.
	Tests string `json:"tests,omitempty"`
}

// Merge.Tests values.
const (
	TestsPassed = "passed"
	TestsFailed = "failed"
)

// Group is a set of tasks whose agents share a group chat and may read
// each other's worktrees. Tasks outside every group are isolated.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon,omitempty"` // an emoji shown before the name, if any
}

// Task is one piece of work for one agent in one worktree.
type Task struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Brief  string     `json:"brief"`           // what the agent is told to do
	Branch string     `json:"branch"`          // new branch for the worktree
	Group  string     `json:"group,omitempty"` // Group.ID, or "" for an isolated task
	Path   string     `json:"path,omitempty"`  // the worktree, once created
	Status TaskStatus `json:"status"`
	Error  string     `json:"error,omitempty"` // why it failed
}

// Settled reports whether every task has finished or failed.
func (p *Plan) Settled() bool {
	for _, t := range p.Tasks {
		if t.Status != Finished && t.Status != Failed {
			return false
		}
	}
	return len(p.Tasks) > 0
}

// Clone copies a plan so it can be edited without touching the original.
func (p *Plan) Clone() *Plan {
	c := *p
	c.Groups = append([]Group(nil), p.Groups...)
	c.Tasks = append([]Task(nil), p.Tasks...)
	if p.Merge != nil {
		mg := *p.Merge
		c.Merge = &mg
	}
	return &c
}

// Task returns the task with the given ID.
func (p *Plan) Task(id string) (*Task, bool) {
	for i := range p.Tasks {
		if p.Tasks[i].ID == id {
			return &p.Tasks[i], true
		}
	}
	return nil, false
}

// Group returns the group with the given ID.
func (p *Plan) Group(id string) (*Group, bool) {
	for i := range p.Groups {
		if p.Groups[i].ID == id {
			return &p.Groups[i], true
		}
	}
	return nil, false
}

// Members lists the tasks in a group, in plan order.
func (p *Plan) Members(group string) []*Task {
	var out []*Task
	for i := range p.Tasks {
		if group != "" && p.Tasks[i].Group == group {
			out = append(out, &p.Tasks[i])
		}
	}
	return out
}

// ChatID is the group ID of the plan chat, which every started plan has:
// the user, the planner and all the plan's agents. Planned groups can't use
// it.
const ChatID = "plan"

// Chat returns the group with the given ID, the plan chat included.
func (p *Plan) Chat(id string) (Group, bool) {
	if id == ChatID {
		return Group{ID: ChatID, Name: "Plan chat"}, true
	}
	if g, ok := p.Group(id); ok {
		return *g, true
	}
	return Group{}, false
}

// Chats lists the plan's chats: its groups, then the plan chat.
func (p *Plan) Chats() []Group {
	g, _ := p.Chat(ChatID)
	return append(append([]Group(nil), p.Groups...), g)
}

// ChatMembers lists the tasks in a chat: every task for the plan chat.
func (p *Plan) ChatMembers(id string) []*Task {
	if id != ChatID {
		return p.Members(id)
	}
	out := make([]*Task, len(p.Tasks))
	for i := range p.Tasks {
		out[i] = &p.Tasks[i]
	}
	return out
}

// Fence is the info string of the code block the planner writes its plan in.
const Fence = "grove-plan"

// proposal is the JSON the planner writes. It is deliberately smaller than
// Plan: the planner proposes, grove fills in IDs, status and times.
type proposal struct {
	Title  string `json:"title"`
	Test   string `json:"test"`
	Groups []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Icon string `json:"icon"`
	} `json:"groups"`
	Tasks []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Brief  string `json:"brief"`
		Branch string `json:"branch"`
		Group  string `json:"group"`
	} `json:"tasks"`
}

// ErrNoPlan means a reply has no grove-plan block.
var ErrNoPlan = errors.New("the reply has no " + Fence + " block")

// FromReply reads the last grove-plan block in a planner reply and turns it
// into a draft plan for request. The result may still have problems for
// Validate to report; the user fixes those before confirming.
func FromReply(reply, request string, now time.Time) (*Plan, error) {
	body, ok := lastFence(reply, Fence)
	if !ok {
		return nil, ErrNoPlan
	}
	var pr proposal
	dec := json.NewDecoder(strings.NewReader(body))
	if err := dec.Decode(&pr); err != nil {
		return nil, fmt.Errorf("the plan isn't valid JSON: %w", err)
	}
	p := &Plan{
		ID:      NewID(now),
		Title:   strings.TrimSpace(pr.Title),
		Test:    strings.TrimSpace(pr.Test),
		Request: request,
		Status:  Draft,
		Created: now,
		Updated: now,
	}
	for _, g := range pr.Groups {
		p.Groups = append(p.Groups, Group{ID: strings.TrimSpace(g.ID), Name: strings.TrimSpace(g.Name), Icon: strings.TrimSpace(g.Icon)})
	}
	for i, t := range pr.Tasks {
		id := strings.TrimSpace(t.ID)
		if id == "" {
			id = fmt.Sprintf("t%d", i+1)
		}
		p.Tasks = append(p.Tasks, Task{
			ID:     id,
			Title:  strings.TrimSpace(t.Title),
			Brief:  strings.TrimSpace(t.Brief),
			Branch: strings.TrimSpace(t.Branch),
			Group:  strings.TrimSpace(t.Group),
			Status: Proposed,
		})
	}
	p.Tidy()
	return p, nil
}

// lastFence returns the body of the last ```info block in s.
func lastFence(s, info string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var body []string
	found, in := false, false
	var cur []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case !in && t == "```"+info:
			in, cur = true, nil
		case in && t == "```":
			in, found, body = false, true, cur
		case in:
			cur = append(cur, l)
		}
	}
	return strings.Join(body, "\n"), found
}

// Tidy renames a group that took the plan chat's ID, and drops groups
// nothing belongs to and groups of one task, which would have nobody to
// talk to, leaving that task isolated.
func (p *Plan) Tidy() {
	for i := range p.Groups {
		if g := &p.Groups[i]; g.ID == ChatID {
			g.ID = ChatID + "-group"
			for j := range p.Tasks {
				if p.Tasks[j].Group == ChatID {
					p.Tasks[j].Group = g.ID
				}
			}
		}
	}
	count := map[string]int{}
	for _, t := range p.Tasks {
		if t.Group != "" {
			count[t.Group]++
		}
	}
	var groups []Group
	for _, g := range p.Groups {
		if count[g.ID] >= 2 {
			groups = append(groups, g)
		}
	}
	p.Groups = groups
	for i := range p.Tasks {
		if _, ok := p.Group(p.Tasks[i].Group); !ok {
			p.Tasks[i].Group = ""
		}
	}
}

// branchRe is a conservative subset of what git accepts as a branch name,
// which is all the planner should ever propose.
var branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// Validate lists what must be fixed before the plan can start. taken holds
// branch names that already exist.
func (p *Plan) Validate(taken []string) []string {
	var probs []string
	if len(p.Tasks) == 0 {
		probs = append(probs, "The plan has no tasks.")
	}
	ids, branches := map[string]bool{}, map[string]bool{}
	for _, b := range taken {
		branches[strings.ToLower(b)] = false
	}
	for i, t := range p.Tasks {
		name := t.Title
		if name == "" {
			name = fmt.Sprintf("Task %d", i+1)
		}
		if ids[t.ID] {
			probs = append(probs, fmt.Sprintf("%s: another task has the ID %q.", name, t.ID))
		}
		ids[t.ID] = true
		if t.Title == "" {
			probs = append(probs, fmt.Sprintf("Task %d has no title.", i+1))
		}
		if t.Brief == "" {
			probs = append(probs, name+": the brief is empty.")
		}
		b := strings.ToLower(t.Branch)
		mine, seen := branches[b]
		switch {
		case t.Branch == "":
			probs = append(probs, name+": no branch name.")
		case !branchRe.MatchString(t.Branch) || strings.Contains(t.Branch, "..") || strings.HasSuffix(t.Branch, ".lock"):
			probs = append(probs, fmt.Sprintf("%s: %q isn't a usable branch name.", name, t.Branch))
		case seen && !mine:
			probs = append(probs, fmt.Sprintf("%s: the branch %s already exists.", name, t.Branch))
		case seen:
			probs = append(probs, fmt.Sprintf("%s: another task also uses the branch %s.", name, t.Branch))
		}
		branches[b] = true
	}
	return probs
}

// NewID makes a plan ID that sorts by creation time.
func NewID(now time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

func file(id string) string { return "plans/" + id + ".json" }

// Save writes the plan to the store, stamping it as updated now.
func Save(s *store.Store, p *Plan, now time.Time) error {
	p.Updated = now
	return s.Save(file(p.ID), p)
}

// Load reads one plan.
func Load(s *store.Store, id string) (*Plan, error) {
	var p Plan
	if err := s.Load(file(id), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// List reads every saved plan, newest first. Plans that can't be read are
// skipped and reported together in the error.
func List(s *store.Store) ([]*Plan, error) {
	names, err := s.List("plans")
	if err != nil {
		return nil, err
	}
	var plans []*Plan
	var errs []error
	for _, n := range names {
		var p Plan
		if err := s.Load(n, &p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n, err))
			continue
		}
		plans = append(plans, &p)
	}
	sort.SliceStable(plans, func(i, j int) bool { return plans[i].Created.After(plans[j].Created) })
	return plans, errors.Join(errs...)
}

// Delete removes a saved plan.
func Delete(s *store.Store, id string) error { return s.Remove(file(id)) }
