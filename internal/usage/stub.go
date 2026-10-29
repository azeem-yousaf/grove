// STUB: stands in for feature/usage-ledger's tokens.go and ledger.go until
// that branch is merged into feature/usage-ui. Delete this file then.
package usage

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type Tokens struct {
	Input, Output, CacheRead, CacheWrite int64
	HasCache                             bool
}

func (t Tokens) Total() int64 { return t.Input + t.Output }

func (t Tokens) Add(o Tokens) Tokens {
	return Tokens{t.Input + o.Input, t.Output + o.Output, t.CacheRead + o.CacheRead, t.CacheWrite + o.CacheWrite, t.HasCache || o.HasCache}
}

func (t Tokens) IsZero() bool { return t == Tokens{} }

type Status uint8

const (
	Unknown Status = iota
	Partial
	Exact
)

type AgentInfo struct {
	ID, Kind, Model, Path, Branch string
	Planner                       bool
	Created                       time.Time
}

type Turn struct {
	ID, Agent                    string
	Time                         time.Time
	Tokens                       Tokens
	Status                       Status
	Failed                       bool
	Plan, PlanTitle, Task, Group string
}

type Assignment struct {
	Plan, PlanTitle string
	Turns           []string
}

type Sum struct {
	Tokens                                 Tokens
	Turns, Exact, Partial, Unknown, Failed int
}

func (s Sum) Complete() bool { return s.Partial == 0 && s.Unknown == 0 }

func (s Sum) Add(o Sum) Sum {
	return Sum{s.Tokens.Add(o.Tokens), s.Turns + o.Turns, s.Exact + o.Exact, s.Partial + o.Partial, s.Unknown + o.Unknown, s.Failed + o.Failed}
}

type AgentSum struct {
	AgentInfo
	Sum Sum
}

type PlanSum struct {
	ID, Title   string
	Sum         Sum
	First, Last time.Time
}

type record struct {
	Agent  *AgentInfo  `json:"agent,omitempty"`
	Turn   *Turn       `json:"turn,omitempty"`
	Assign *Assignment `json:"assign,omitempty"`
}

type Ledger struct {
	mu     sync.Mutex
	file   string
	agents map[string]AgentInfo
	turns  []Turn
	ids    map[string]int
}

func Open(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &Ledger{file: filepath.Join(dir, "ledger.jsonl")}
	return l, l.Reload()
}

func (l *Ledger) write(r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (l *Ledger) apply(r record) bool {
	switch {
	case r.Agent != nil:
		l.agents[r.Agent.ID] = *r.Agent
	case r.Turn != nil:
		if _, ok := l.ids[r.Turn.ID]; ok {
			return false
		}
		l.ids[r.Turn.ID] = len(l.turns)
		l.turns = append(l.turns, *r.Turn)
	case r.Assign != nil:
		for _, id := range r.Assign.Turns {
			if i, ok := l.ids[id]; ok && l.turns[i].Plan == "" {
				l.turns[i].Plan, l.turns[i].PlanTitle = r.Assign.Plan, r.Assign.PlanTitle
			}
		}
	}
	return true
}

func (l *Ledger) Reload() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.agents, l.turns, l.ids = map[string]AgentInfo{}, nil, map[string]int{}
	f, err := os.Open(l.file)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			l.apply(r)
		}
	}
	return sc.Err()
}

func (l *Ledger) SetAgent(a AgentInfo) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.apply(record{Agent: &a})
	return l.write(record{Agent: &a})
}

func (l *Ledger) Add(t Turn) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.Time.IsZero() {
		t.Time = time.Now()
	}
	if !l.apply(record{Turn: &t}) {
		return false, nil
	}
	return true, l.write(record{Turn: &t})
}

func (l *Ledger) Assign(a Assignment) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.apply(record{Assign: &a})
	return l.write(record{Assign: &a})
}

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

func (l *Ledger) sum(keep func(Turn) bool) Sum {
	l.mu.Lock()
	defer l.mu.Unlock()
	var s Sum
	for _, t := range l.turns {
		if keep(t) {
			s = s.Add(sumOf(t))
		}
	}
	return s
}

func (l *Ledger) Total() Sum {
	return l.sum(func(Turn) bool { return true })
}

func (l *Ledger) Agent(id string) Sum {
	return l.sum(func(t Turn) bool { return t.Agent == id })
}

func (l *Ledger) Plan(id string) Sum {
	return l.sum(func(t Turn) bool { return t.Plan == id })
}

func (l *Ledger) Unassigned() Sum {
	return l.sum(func(t Turn) bool { return t.Plan == "" && l.agents[t.Agent].Planner })
}

func (l *Ledger) Group(plan, g string) Sum {
	return l.sum(func(t Turn) bool { return t.Plan == plan && t.Group == g })
}
func (l *Ledger) Task(plan, task string) Sum {
	return l.sum(func(t Turn) bool { return t.Plan == plan && t.Task == task })
}

func (l *Ledger) Planner() Sum { return l.sum(func(t Turn) bool { return l.agents[t.Agent].Planner }) }

// PlannerIn is the planner's turns for plan; "" for its turns with no plan.
func (l *Ledger) PlannerIn(plan string) Sum {
	return l.sum(func(t Turn) bool { return t.Plan == plan && l.agents[t.Agent].Planner })
}

func (a AgentInfo) Name() string {
	switch {
	case a.Branch != "":
		return a.Branch
	case a.Path != "":
		return filepath.Base(a.Path)
	}
	return a.ID
}

func (l *Ledger) AgentInfo(id string) (AgentInfo, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.agents[id]
	return a, ok
}

func (l *Ledger) Agents() []AgentSum {
	l.mu.Lock()
	by := map[string]*AgentSum{}
	for _, t := range l.turns {
		a := by[t.Agent]
		if a == nil {
			a = &AgentSum{AgentInfo: l.agents[t.Agent]}
			a.ID = t.Agent
			by[t.Agent] = a
		}
		a.Sum = a.Sum.Add(sumOf(t))
	}
	l.mu.Unlock()
	var out []AgentSum
	for _, a := range by {
		out = append(out, *a)
	}
	slices.SortFunc(out, func(a, b AgentSum) int { return int(b.Sum.Tokens.Total() - a.Sum.Tokens.Total()) })
	return out
}

func (l *Ledger) Plans() []PlanSum {
	l.mu.Lock()
	by := map[string]*PlanSum{}
	for _, t := range l.turns {
		if t.Plan == "" {
			continue
		}
		p := by[t.Plan]
		if p == nil {
			p = &PlanSum{ID: t.Plan, First: t.Time}
			by[t.Plan] = p
		}
		if t.PlanTitle != "" {
			p.Title = t.PlanTitle
		}
		if t.Time.After(p.Last) {
			p.Last = t.Time
		}
		p.Sum = p.Sum.Add(sumOf(t))
	}
	l.mu.Unlock()
	var out []PlanSum
	for _, p := range by {
		out = append(out, *p)
	}
	slices.SortFunc(out, func(a, b PlanSum) int { return b.Last.Compare(a.Last) })
	return out
}

func Compact(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprint(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	}
}

func FormatSum(s Sum) string {
	if s.Turns > 0 && s.Unknown == s.Turns {
		return "n/a"
	}
	out := Compact(s.Tokens.Total())
	if !s.Complete() {
		out = "≥" + out
	}
	return out
}
