// Package usage records how many tokens grove's agents use and adds them
// up per agent, plan, group and task. It knows nothing about providers:
// callers hand it Tokens already read from a CLI's output.
//
// Everything lives in one append-only JSON-lines file, so history survives
// restarts, removed worktrees and finished plans. Several grove processes can
// share it: writes happen under a lock file and Reload picks up their lines.
package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/azeem-yousaf/grove/internal/fslock"
)

// version is written into every line.
const version = 1

// AgentInfo describes one agent conversation. ID is grove's own id for it,
// never the provider's session id.
type AgentInfo struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind,omitempty"`
	Model   string    `json:"model,omitempty"`
	Path    string    `json:"path,omitempty"`
	Branch  string    `json:"branch,omitempty"`
	Planner bool      `json:"planner,omitempty"`
	Created time.Time `json:"created,omitzero"`
}

// Name is how the agent is shown: its branch, else its folder, else its ID.
func (a AgentInfo) Name() string {
	switch {
	case a.Branch != "":
		return a.Branch
	case a.Path != "":
		return filepath.Base(a.Path)
	}
	return a.ID
}

// Turn is what one agent turn used. ID is unique per turn, e.g. agentID+"/"+n.
type Turn struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	Time      time.Time `json:"time,omitzero"`
	Tokens    Tokens    `json:"tokens"`
	Status    Status    `json:"status"`
	Failed    bool      `json:"failed,omitempty"`
	Plan      string    `json:"plan,omitempty"`
	PlanTitle string    `json:"plan_title,omitempty"`
	Task      string    `json:"task,omitempty"`
	Group     string    `json:"group,omitempty"`
	Kind      Kind      `json:"kind,omitempty"`
}

// Kind is why a turn happened. Lines written before kinds existed have none
// and count as KindUnknown.
type Kind string

const (
	KindWork    Kind = "work"   // a user's or plan's message to an agent
	KindResume  Kind = "resume" // an agent carried on after approvals
	KindWake    Kind = "wake"   // woken for group chat messages
	KindDecide  Kind = "decide" // a planner's orchestration turn
	KindPlan    Kind = "plan"   // the planner drafting from the user's request
	KindFix     Kind = "fix"    // fixing merge conflicts or failed tests
	KindUnknown Kind = "unknown"
)

// Kinds is every kind in the order they are shown.
var Kinds = []Kind{KindWork, KindResume, KindWake, KindDecide, KindPlan, KindFix, KindUnknown}

// KindSum is one kind's total.
type KindSum struct {
	Kind Kind
	Sum  Sum
}

// Assignment gives turns recorded with no Plan to a plan later, such as the
// planner's turns spent drafting it.
type Assignment struct {
	Plan      string   `json:"plan"`
	PlanTitle string   `json:"plan_title,omitempty"`
	Turns     []string `json:"turns"`
}

// Sum adds up turns.
type Sum struct {
	Tokens  Tokens
	Turns   int
	Exact   int
	Partial int
	Unknown int
	Failed  int
}

// Complete reports whether every turn has an exact figure.
func (s Sum) Complete() bool { return s.Partial == 0 && s.Unknown == 0 }

// Add returns both sums together.
func (s Sum) Add(o Sum) Sum {
	return Sum{
		Tokens:  s.Tokens.Add(o.Tokens),
		Turns:   s.Turns + o.Turns,
		Exact:   s.Exact + o.Exact,
		Partial: s.Partial + o.Partial,
		Unknown: s.Unknown + o.Unknown,
		Failed:  s.Failed + o.Failed,
	}
}

// sub takes o back out of s; HasCache stays as it was.
func (s Sum) sub(o Sum) Sum {
	t := s.Tokens
	t.Input -= o.Tokens.Input
	t.Output -= o.Tokens.Output
	t.CacheRead -= o.Tokens.CacheRead
	t.CacheWrite -= o.Tokens.CacheWrite
	return Sum{
		Tokens:  t,
		Turns:   s.Turns - o.Turns,
		Exact:   s.Exact - o.Exact,
		Partial: s.Partial - o.Partial,
		Unknown: s.Unknown - o.Unknown,
		Failed:  s.Failed - o.Failed,
	}
}

// sumOf is one turn as a Sum.
func sumOf(t Turn) Sum {
	s := Sum{Tokens: t.Tokens, Turns: 1}
	switch t.Status {
	case Exact:
		s.Exact = 1
	case Partial:
		s.Partial = 1
	default:
		s.Unknown = 1
	}
	if t.Failed {
		s.Failed = 1
	}
	return s
}

// AgentSum is one agent's total.
type AgentSum struct {
	AgentInfo
	Sum Sum
}

// PlanSum is one plan's total, planner turns included.
type PlanSum struct {
	ID    string
	Title string // the last one recorded
	Sum   Sum
	First time.Time
	Last  time.Time
}

// record is one line of the file.
type record struct {
	V      int         `json:"v"`
	Agent  *AgentInfo  `json:"agent,omitempty"`
	Turn   *Turn       `json:"turn,omitempty"`
	Assign *Assignment `json:"assign,omitempty"`
}

type planTotals struct {
	PlanSum
	groups    map[string]Sum
	tasks     map[string]Sum
	taskGroup map[string]string
}

// Ledger is the usage record of one repository. It is safe for concurrent use.
type Ledger struct {
	dir string

	// wmu serialises this process's writers, which wait for the file lock
	// without holding mu, so readers never wait for another grove.
	wmu sync.Mutex

	mu      sync.Mutex
	offset  int64 // bytes of the file read, always at a line start
	skipped int

	agents   map[string]AgentInfo
	turns    map[string]Turn       // with the plan they count towards
	pending  map[string]Assignment // assignments naming turns not seen yet
	total    Sum
	byAgent  map[string]Sum
	byKind   map[Kind]Sum
	agentIn  map[string]map[string]Sum // agent → plan ("" for none) → turns
	plans    map[string]*planTotals
	unplaced Sum
}

// Open reads the ledger in dir (<store>/usage). The folder is created on
// the first write; a missing one is an empty ledger.
func Open(dir string) (*Ledger, error) {
	l := &Ledger{dir: dir}
	l.reset()
	if err := l.Reload(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Ledger) reset() {
	l.offset, l.skipped = 0, 0
	l.agents = map[string]AgentInfo{}
	l.turns = map[string]Turn{}
	l.pending = map[string]Assignment{}
	l.total, l.unplaced = Sum{}, Sum{}
	l.byAgent = map[string]Sum{}
	l.byKind = map[Kind]Sum{}
	l.agentIn = map[string]map[string]Sum{}
	l.plans = map[string]*planTotals{}
}

func (l *Ledger) file() string { return filepath.Join(l.dir, "ledger.jsonl") }

// Skipped is how many lines could not be read: malformed, or with negative
// numbers.
func (l *Ledger) Skipped() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.skipped
}

// Reload reads lines appended since the last read, by this or another grove
// process. It only stats the file when nothing changed.
func (l *Ledger) Reload() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readNew()
}

// readNew applies complete lines after l.offset. A last line without its
// newline is left for later: it is either being written, or a crash's
// leftover that the next append closes.
func (l *Ledger) readNew() error {
	fi, err := os.Stat(l.file())
	if errors.Is(err, os.ErrNotExist) {
		if l.offset > 0 {
			l.reset()
		}
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Size() == l.offset {
		return nil
	}
	if fi.Size() < l.offset {
		// The file never shrinks unless someone replaced it: start over.
		l.reset()
	}
	f, err := os.Open(l.file())
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(l.offset, io.SeekStart); err != nil {
		return err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	end := bytes.LastIndexByte(b, '\n')
	if end < 0 {
		return nil
	}
	for line := range bytes.SplitSeq(b[:end], []byte{'\n'}) {
		l.applyLine(line)
	}
	l.offset += int64(end + 1)
	return nil
}

func (l *Ledger) applyLine(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		l.skipped++
		return
	}
	switch {
	case r.Agent != nil && r.Agent.ID != "":
		l.agents[r.Agent.ID] = *r.Agent
	case r.Turn != nil && validTurn(*r.Turn) == nil:
		l.applyTurn(*r.Turn)
	case r.Assign != nil && r.Assign.Plan != "":
		l.applyAssign(*r.Assign)
	default:
		l.skipped++
	}
}

func validTurn(t Turn) error {
	if t.ID == "" {
		return errors.New("usage: turn without an ID")
	}
	k := t.Tokens
	if k.Input < 0 || k.Output < 0 || k.CacheRead < 0 || k.CacheWrite < 0 {
		return fmt.Errorf("usage: turn %s has negative token counts", t.ID)
	}
	return nil
}

func (l *Ledger) applyTurn(t Turn) {
	if _, dup := l.turns[t.ID]; dup {
		return
	}
	if t.Plan == "" {
		if a, ok := l.pending[t.ID]; ok {
			t.Plan, t.PlanTitle = a.Plan, a.PlanTitle
			t.Task, t.Group = "", ""
		}
	}
	delete(l.pending, t.ID)
	l.turns[t.ID] = t
	s := sumOf(t)
	l.total = l.total.Add(s)
	l.byAgent[t.Agent] = l.byAgent[t.Agent].Add(s)
	k := t.Kind
	if k == "" {
		k = KindUnknown
	}
	l.byKind[k] = l.byKind[k].Add(s)
	in := l.agentIn[t.Agent]
	if in == nil {
		in = map[string]Sum{}
		l.agentIn[t.Agent] = in
	}
	in[t.Plan] = in[t.Plan].Add(s)
	if t.Plan == "" {
		l.unplaced = l.unplaced.Add(s)
		return
	}
	l.addToPlan(t, s)
}

func (l *Ledger) addToPlan(t Turn, s Sum) {
	p := l.plans[t.Plan]
	if p == nil {
		p = &planTotals{
			PlanSum:   PlanSum{ID: t.Plan},
			groups:    map[string]Sum{},
			tasks:     map[string]Sum{},
			taskGroup: map[string]string{},
		}
		l.plans[t.Plan] = p
	}
	if t.PlanTitle != "" {
		p.Title = t.PlanTitle
	}
	p.Sum = p.Sum.Add(s)
	if !t.Time.IsZero() {
		if p.First.IsZero() || t.Time.Before(p.First) {
			p.First = t.Time
		}
		if t.Time.After(p.Last) {
			p.Last = t.Time
		}
	}
	if t.Group != "" {
		p.groups[t.Group] = p.groups[t.Group].Add(s)
	}
	if t.Task != "" {
		p.tasks[t.Task] = p.tasks[t.Task].Add(s)
		p.taskGroup[t.Task] = t.Group
	}
}

// applyAssign moves unplaced turns into the plan. Turns that already count
// towards a plan, their own or an earlier assignment's, stay where they are.
func (l *Ledger) applyAssign(a Assignment) {
	for _, id := range a.Turns {
		t, ok := l.turns[id]
		if !ok {
			if _, taken := l.pending[id]; !taken {
				l.pending[id] = a
			}
			continue
		}
		if t.Plan != "" {
			continue
		}
		s := sumOf(t)
		l.unplaced = l.unplaced.sub(s)
		in := l.agentIn[t.Agent]
		in[""] = in[""].sub(s)
		in[a.Plan] = in[a.Plan].Add(s)
		t.Plan, t.PlanTitle = a.Plan, a.PlanTitle
		l.turns[id] = t
		l.addToPlan(t, s)
	}
	if p := l.plans[a.Plan]; p != nil && a.PlanTitle != "" {
		p.Title = a.PlanTitle
	}
}

// write appends one record under the lock, after catching up with other
// writers. check, called once caught up, says whether to write at all.
// Waiting for the lock holds only wmu; mu is held while reading, checking
// and appending, so queries never wait for another process.
func (l *Ledger) write(r record, check func() bool) (bool, error) {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return false, err
	}
	unlock, err := fslock.Lock(filepath.Join(l.dir, "lock"))
	if errors.Is(err, fslock.ErrBusy) {
		return false, errors.New("the usage ledger is locked by another program")
	}
	if err != nil {
		return false, err
	}
	defer unlock()
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.readNew(); err != nil {
		return false, err
	}
	if !check() {
		return false, nil
	}
	r.V = version
	b, err := json.Marshal(r)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(l.file(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	// Unread bytes past the offset, while we hold the lock, are a line a
	// crash cut short: end it so this record starts on its own line.
	if fi, err := f.Stat(); err == nil && fi.Size() > l.offset {
		b = append([]byte{'\n'}, b...)
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}
	return true, l.readNew()
}

// SetAgent records an agent's details, appending only when they differ from
// what is recorded.
func (l *Ledger) SetAgent(a AgentInfo) error {
	if a.ID == "" {
		return errors.New("usage: agent without an ID")
	}
	_, err := l.write(record{Agent: &a}, func() bool {
		old, ok := l.agents[a.ID]
		return !ok || !sameAgent(old, a)
	})
	return err
}

func sameAgent(a, b AgentInfo) bool {
	ca, cb := a.Created, b.Created
	a.Created, b.Created = time.Time{}, time.Time{}
	return a == b && ca.Equal(cb)
}

// Add records a turn. A turn whose ID is already recorded is ignored and
// added is false.
func (l *Ledger) Add(t Turn) (added bool, err error) {
	if err := validTurn(t); err != nil {
		return false, err
	}
	return l.write(record{Turn: &t}, func() bool {
		_, dup := l.turns[t.ID]
		return !dup
	})
}

// Assign gives turns with no plan of their own to a plan.
func (l *Ledger) Assign(a Assignment) error {
	if a.Plan == "" {
		return errors.New("usage: assignment without a plan")
	}
	_, err := l.write(record{Assign: &a}, func() bool {
		for _, id := range a.Turns {
			if t, ok := l.turns[id]; !ok || t.Plan == "" {
				return true
			}
		}
		return false
	})
	return err
}

// Total is every recorded turn.
func (l *Ledger) Total() Sum {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// Agent is one agent's turns.
func (l *Ledger) Agent(id string) Sum {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byAgent[id]
}

// AgentInfo is what SetAgent last recorded for id.
func (l *Ledger) AgentInfo(id string) (AgentInfo, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.agents[id]
	return a, ok
}

// Plan is every turn attributed to a plan, the planner's included.
func (l *Ledger) Plan(id string) Sum {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p := l.plans[id]; p != nil {
		return p.Sum
	}
	return Sum{}
}

// Group is the turns of one plan group's tasks.
func (l *Ledger) Group(plan, group string) Sum {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p := l.plans[plan]; p != nil {
		return p.groups[group]
	}
	return Sum{}
}

// Task is the turns of one plan task.
func (l *Ledger) Task(plan, task string) Sum {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p := l.plans[plan]; p != nil {
		return p.tasks[task]
	}
	return Sum{}
}

// Planner is every turn of planner agents.
func (l *Ledger) Planner() Sum {
	l.mu.Lock()
	defer l.mu.Unlock()
	var s Sum
	for id, a := range l.agents {
		if a.Planner {
			s = s.Add(l.byAgent[id])
		}
	}
	return s
}

// PlannerIn is the planner agents' turns that count towards plan; "" is
// their turns with no plan.
func (l *Ledger) PlannerIn(plan string) Sum {
	l.mu.Lock()
	defer l.mu.Unlock()
	var s Sum
	for id, a := range l.agents {
		if a.Planner {
			s = s.Add(l.agentIn[id][plan])
		}
	}
	return s
}

// Unassigned is the turns that count towards no plan.
func (l *Ledger) Unassigned() Sum {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unplaced
}

// ByKind lists every kind with turns, in the order of Kinds; a kind not in
// Kinds (from a newer grove) comes after them, by name.
func (l *Ledger) ByKind() []KindSum {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]KindSum, 0, len(l.byKind))
	for _, k := range Kinds {
		if s, ok := l.byKind[k]; ok {
			out = append(out, KindSum{Kind: k, Sum: s})
		}
	}
	var other []KindSum
	for k, s := range l.byKind {
		if !slices.Contains(Kinds, k) {
			other = append(other, KindSum{Kind: k, Sum: s})
		}
	}
	sort.Slice(other, func(i, j int) bool { return other[i].Kind < other[j].Kind })
	return append(out, other...)
}

// Agents lists every agent with turns, the most tokens first, ties by name.
func (l *Ledger) Agents() []AgentSum {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]AgentSum, 0, len(l.byAgent))
	for id, s := range l.byAgent {
		a, ok := l.agents[id]
		if !ok {
			a = AgentInfo{ID: id}
		}
		out = append(out, AgentSum{AgentInfo: a, Sum: s})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if x, y := a.Sum.Tokens.Total(), b.Sum.Tokens.Total(); x != y {
			return x > y
		}
		if a.Name() != b.Name() {
			return a.Name() < b.Name()
		}
		return a.ID < b.ID
	})
	return out
}

// Plans lists every plan with turns, the most recently active first.
func (l *Ledger) Plans() []PlanSum {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]PlanSum, 0, len(l.plans))
	for _, p := range l.plans {
		out = append(out, p.PlanSum)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// GroupSum is one plan group's total.
type GroupSum struct {
	Group string
	Sum   Sum
}

// TaskSum is one plan task's total, with the group its turns named last.
type TaskSum struct {
	Task  string
	Group string
	Sum   Sum
}

// Groups lists a plan's groups with turns, by name.
func (l *Ledger) Groups(plan string) []GroupSum {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.plans[plan]
	if p == nil {
		return nil
	}
	out := make([]GroupSum, 0, len(p.groups))
	for g, s := range p.groups {
		out = append(out, GroupSum{Group: g, Sum: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}

// Tasks lists a plan's tasks with turns, by name.
func (l *Ledger) Tasks(plan string) []TaskSum {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.plans[plan]
	if p == nil {
		return nil
	}
	out := make([]TaskSum, 0, len(p.tasks))
	for t, s := range p.tasks {
		out = append(out, TaskSum{Task: t, Group: p.taskGroup[t], Sum: s})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	return out
}
