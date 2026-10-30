package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/azeem-yousaf/grove/internal/fslock"
)

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func open(t *testing.T, dir string) *Ledger {
	t.Helper()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func add(t *testing.T, l *Ledger, turn Turn) bool {
	t.Helper()
	ok, err := l.Add(turn)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func exact(id, agent string, in, out int64) Turn {
	return Turn{ID: id, Agent: agent, Time: t0, Tokens: Tokens{Input: in, Output: out}, Status: Exact}
}

func TestTokens(t *testing.T) {
	a := Tokens{Input: 100, Output: 10, CacheRead: 60, CacheWrite: 5, HasCache: true}
	b := Tokens{Input: 50, Output: 5}
	s := a.Add(b)
	if s.Total() != 165 || s.CacheRead != 60 || !s.HasCache {
		t.Errorf("Add = %+v", s)
	}
	if b.Add(Tokens{}).HasCache {
		t.Error("HasCache from nowhere")
	}
	if !(Tokens{}).IsZero() || a.IsZero() {
		t.Error("IsZero")
	}
	for _, st := range []Status{Unknown, Partial, Exact} {
		b, _ := json.Marshal(st)
		var back Status = 99
		if err := json.Unmarshal(b, &back); err != nil || back != st {
			t.Errorf("%v round trip: %s → %v", st, b, back)
		}
	}
	var st Status = Exact
	if err := st.UnmarshalText([]byte("weird")); err != nil || st != Unknown {
		t.Errorf("unknown text gave %v", st)
	}
}

func TestTotals(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir)
	must(t, l.SetAgent(AgentInfo{ID: "p1", Kind: "claude", Planner: true, Path: "/r/main", Branch: "master"}))
	must(t, l.SetAgent(AgentInfo{ID: "a1", Kind: "claude", Branch: "feature/a"}))
	must(t, l.SetAgent(AgentInfo{ID: "a2", Kind: "codex", Branch: "feature/b"}))

	add(t, l, exact("p1/1", "p1", 1000, 100))
	pl := exact("p1/2", "p1", 500, 50)
	pl.Plan, pl.PlanTitle = "plan1", "First plan"
	add(t, l, pl)
	ta := exact("a1/1", "a1", 2000, 200)
	ta.Plan, ta.PlanTitle, ta.Task, ta.Group = "plan1", "First plan", "feature/a", "g1"
	ta.Tokens.CacheRead, ta.Tokens.HasCache = 1500, true
	add(t, l, ta)
	tb := exact("a2/1", "a2", 300, 30)
	tb.Plan, tb.Task, tb.Group = "plan1", "feature/b", "g1"
	tb.Time = t0.Add(time.Hour)
	add(t, l, tb)
	add(t, l, exact("a2/2", "a2", 10, 1))

	if got := l.Total(); got.Tokens.Total() != 4191 || got.Turns != 5 || !got.Complete() || got.Exact != 5 {
		t.Errorf("Total = %+v", got)
	}
	if got := l.Plan("plan1"); got.Tokens.Total() != 3080 || got.Turns != 3 || !got.Tokens.HasCache {
		t.Errorf("Plan = %+v", got)
	}
	if got := l.Group("plan1", "g1"); got.Tokens.Total() != 2530 {
		t.Errorf("Group = %+v", got)
	}
	if got := l.Task("plan1", "feature/b"); got.Tokens.Total() != 330 || got.Tokens.HasCache {
		t.Errorf("Task = %+v", got)
	}
	if got := l.Planner(); got.Tokens.Total() != 1650 {
		t.Errorf("Planner = %+v", got)
	}
	if got := l.PlannerIn("plan1"); got.Tokens.Total() != 550 {
		t.Errorf("PlannerIn(plan1) = %+v", got)
	}
	if got := l.PlannerIn(""); got.Tokens.Total() != 1100 {
		t.Errorf("PlannerIn(\"\") = %+v", got)
	}
	if got := l.Unassigned(); got.Tokens.Total() != 1111 || got.Turns != 2 {
		t.Errorf("Unassigned = %+v", got)
	}
	if got := l.Agent("a2"); got.Turns != 2 {
		t.Errorf("Agent(a2) = %+v", got)
	}
	agents := l.Agents()
	if len(agents) != 3 || agents[0].ID != "a1" || agents[1].ID != "p1" || agents[2].Name() != "feature/b" {
		t.Errorf("Agents = %+v", agents)
	}
	plans := l.Plans()
	if len(plans) != 1 || plans[0].Title != "First plan" || !plans[0].First.Equal(t0) || !plans[0].Last.Equal(t0.Add(time.Hour)) {
		t.Errorf("Plans = %+v", plans)
	}
	if g := l.Groups("plan1"); len(g) != 1 || g[0].Group != "g1" {
		t.Errorf("Groups = %+v", g)
	}
	if ts := l.Tasks("plan1"); len(ts) != 2 || ts[0].Task != "feature/a" || ts[0].Group != "g1" {
		t.Errorf("Tasks = %+v", ts)
	}

	// Reopening reads the same totals back.
	r := open(t, dir)
	if r.Total() != l.Total() || r.Plan("plan1") != l.Plan("plan1") || r.Planner() != l.Planner() ||
		r.Unassigned() != l.Unassigned() || r.Group("plan1", "g1") != l.Group("plan1", "g1") || len(r.Agents()) != 3 {
		t.Errorf("reopened totals differ: %+v vs %+v", r.Total(), l.Total())
	}
	if a, ok := r.AgentInfo("p1"); !ok || !a.Planner || a.Path != "/r/main" {
		t.Errorf("AgentInfo = %+v %v", a, ok)
	}
	if r.Skipped() != 0 {
		t.Errorf("Skipped = %d", r.Skipped())
	}
}

func TestDuplicateTurns(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir)
	if !add(t, l, exact("a/1", "a", 10, 1)) {
		t.Fatal("first add refused")
	}
	if add(t, l, exact("a/1", "a", 999, 1)) {
		t.Error("duplicate added")
	}
	// A duplicate line written by another process counts once too.
	line, _ := json.Marshal(record{V: 1, Turn: &Turn{ID: "a/1", Agent: "a", Tokens: Tokens{Input: 5}, Status: Exact}})
	appendRaw(t, dir, string(line)+"\n")
	must(t, l.Reload())
	if got := l.Total(); got.Turns != 1 || got.Tokens.Total() != 11 {
		t.Errorf("Total = %+v", got)
	}
	if got := open(t, dir).Total(); got.Turns != 1 || got.Tokens.Total() != 11 {
		t.Errorf("reopened Total = %+v", got)
	}
	if _, err := l.Add(Turn{ID: "a/2", Tokens: Tokens{Input: -1}}); err == nil {
		t.Error("negative tokens accepted")
	}
	if _, err := l.Add(Turn{Agent: "a"}); err == nil {
		t.Error("turn without ID accepted")
	}
}

func TestAssign(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir)
	must(t, l.SetAgent(AgentInfo{ID: "p", Planner: true}))
	add(t, l, exact("p/1", "p", 100, 10))
	add(t, l, exact("p/2", "p", 200, 20))
	own := exact("p/3", "p", 300, 30)
	own.Plan = "other"
	add(t, l, own)

	must(t, l.Assign(Assignment{Plan: "x", PlanTitle: "X", Turns: []string{"p/1", "p/3"}}))
	must(t, l.Assign(Assignment{Plan: "x", PlanTitle: "X", Turns: []string{"p/1", "p/3"}}))
	// The first assignment wins.
	must(t, l.Assign(Assignment{Plan: "y", Turns: []string{"p/1"}}))
	// Assigning a turn recorded later still works.
	must(t, l.Assign(Assignment{Plan: "x", Turns: []string{"p/4"}}))
	add(t, l, exact("p/4", "p", 1, 0))

	check := func(l *Ledger) {
		t.Helper()
		if got := l.Plan("x"); got.Turns != 2 || got.Tokens.Total() != 111 {
			t.Errorf("Plan(x) = %+v", got)
		}
		if got := l.Plan("other"); got.Turns != 1 {
			t.Errorf("Plan(other) = %+v", got)
		}
		if got := l.Plan("y"); got.Turns != 0 {
			t.Errorf("Plan(y) = %+v", got)
		}
		if got := l.Unassigned(); got.Turns != 1 || got.Tokens.Total() != 220 {
			t.Errorf("Unassigned = %+v", got)
		}
		if got := l.PlannerIn("x"); got.Turns != 2 {
			t.Errorf("PlannerIn(x) = %+v", got)
		}
		if got := l.PlannerIn(""); got.Turns != 1 {
			t.Errorf("PlannerIn(\"\") = %+v", got)
		}
		if got := l.Total(); got.Turns != 4 {
			t.Errorf("Total = %+v", got)
		}
		if p := l.Plans(); len(p) != 2 {
			t.Errorf("Plans = %+v", p)
		}
	}
	check(l)
	check(open(t, dir))
}

func TestIncomplete(t *testing.T) {
	l := open(t, t.TempDir())
	add(t, l, exact("a/1", "a", 100, 10))
	add(t, l, Turn{ID: "a/2", Agent: "a", Tokens: Tokens{Input: 50, Output: 5}, Status: Partial, Failed: true})
	add(t, l, Turn{ID: "b/1", Agent: "b", Status: Unknown})

	s := l.Total()
	if s.Complete() || s.Exact != 1 || s.Partial != 1 || s.Unknown != 1 || s.Failed != 1 || s.Turns != 3 {
		t.Errorf("Total = %+v", s)
	}
	if s.Tokens.Total() != 165 {
		t.Errorf("tokens = %d", s.Tokens.Total())
	}
	if l.Agent("a").Complete() {
		t.Error("agent a should be incomplete")
	}
	if b := l.Agent("b"); b.Complete() || Amount(b, b.Tokens.Total()) != "?" {
		t.Errorf("unknown-only agent shows %q", Amount(b, b.Tokens.Total()))
	}
	if got := FormatSum(s); got != "≥165 tokens in 3 turns (1 turn cut short, 1 not reported)" {
		t.Errorf("FormatSum = %q", got)
	}
}

func TestReloadOtherWriter(t *testing.T) {
	dir := t.TempDir()
	a, b := open(t, dir), open(t, dir)
	add(t, a, exact("a/1", "a", 10, 1))
	add(t, b, exact("b/1", "b", 20, 2))
	if got := b.Total(); got.Turns != 2 {
		t.Errorf("b caught up on its write: %+v", got)
	}
	if got := a.Total(); got.Turns != 1 {
		t.Errorf("a before Reload: %+v", got)
	}
	must(t, a.Reload())
	must(t, a.Reload())
	if got := a.Total(); got.Turns != 2 || got.Tokens.Total() != 33 {
		t.Errorf("a after Reload: %+v", got)
	}
	// The same turn written by both counts once.
	if add(t, a, exact("b/1", "b", 20, 2)) {
		t.Error("b's turn added again by a")
	}
}

func TestBadFiles(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "usage")
		l := open(t, dir)
		if l.Total().Turns != 0 {
			t.Error("not empty")
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Error("Open created the folder")
		}
		add(t, l, exact("a/1", "a", 1, 1))
		if open(t, dir).Total().Turns != 1 {
			t.Error("not written")
		}
	})
	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		appendRaw(t, dir, "")
		l := open(t, dir)
		if l.Total().Turns != 0 || l.Skipped() != 0 {
			t.Errorf("Total %+v, Skipped %d", l.Total(), l.Skipped())
		}
	})
	t.Run("malformed", func(t *testing.T) {
		dir := t.TempDir()
		good, _ := json.Marshal(record{V: 1, Turn: &Turn{ID: "a/1", Agent: "a", Tokens: Tokens{Input: 7}, Status: Exact}})
		appendRaw(t, dir, strings.Join([]string{
			"not json",
			string(good),
			`{"v":1,"turn":{"id":"a/2","agent":"a","tokens":{"input":-5,"output":1},"status":"exact"}}`,
			`{"v":1}`,
			`{"v":1,"turn":{"agent":"a"}}`,
			"",
		}, "\n")+"\n")
		l := open(t, dir)
		if l.Total().Turns != 1 || l.Skipped() != 4 {
			t.Errorf("Total %+v, Skipped %d", l.Total(), l.Skipped())
		}
	})
	t.Run("truncated", func(t *testing.T) {
		dir := t.TempDir()
		good, _ := json.Marshal(record{V: 1, Turn: &Turn{ID: "a/1", Agent: "a", Tokens: Tokens{Input: 7}, Status: Exact}})
		appendRaw(t, dir, string(good)+"\n"+`{"v":1,"turn":{"id":"a/2","ag`)
		l := open(t, dir)
		if l.Total().Turns != 1 {
			t.Errorf("Total %+v", l.Total())
		}
		add(t, l, exact("a/3", "a", 1, 0))
		r := open(t, dir)
		if r.Total().Turns != 2 || r.Skipped() != 1 {
			t.Errorf("after append: Total %+v, Skipped %d", r.Total(), r.Skipped())
		}
		b, _ := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
		if !strings.HasSuffix(string(b), "\n") || strings.Count(string(b), "\n") != 3 {
			t.Errorf("file:\n%s", b)
		}
	})
}

func TestSetAgent(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir)
	// The same branch at different times is two agents.
	must(t, l.SetAgent(AgentInfo{ID: "x1", Branch: "feature/a", Created: t0}))
	must(t, l.SetAgent(AgentInfo{ID: "x2", Branch: "feature/a", Created: t0.Add(time.Hour)}))
	must(t, l.SetAgent(AgentInfo{ID: "x1", Branch: "feature/a", Created: t0.In(time.Local)}))
	add(t, l, exact("x1/1", "x1", 10, 0))
	add(t, l, exact("x2/1", "x2", 20, 0))
	if l.Agent("x1").Tokens.Total() != 10 || l.Agent("x2").Tokens.Total() != 20 || len(l.Agents()) != 2 {
		t.Errorf("agents merged: %+v", l.Agents())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if n := strings.Count(string(b), `"agent":{`); n != 2 {
		t.Errorf("%d agent lines, want 2 (an unchanged SetAgent appends nothing)", n)
	}
	must(t, l.SetAgent(AgentInfo{ID: "x1", Branch: "feature/a", Created: t0, Model: "opus"}))
	if a, _ := open(t, dir).AgentInfo("x1"); a.Model != "opus" {
		t.Errorf("AgentInfo = %+v", a)
	}
}

func TestCompact(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0", 950: "950", 999: "999", 1000: "1k", 1234: "1.23k", 12345: "12.3k",
		123456: "123k", 999_999: "1M", 4_560_000: "4.56M", 1_200_000_000: "1.2G",
	} {
		if got := Compact(n); got != want {
			t.Errorf("Compact(%d) = %q, want %q", n, got, want)
		}
	}
	s := Sum{Tokens: Tokens{Input: 1000}, Turns: 1, Exact: 1}
	if Cached(s) != "-" || FormatSum(s) != "1k tokens in 1 turn" || FormatSum(Sum{}) != "no turns" {
		t.Errorf("Cached %q, FormatSum %q", Cached(s), FormatSum(s))
	}
	if got := FormatSum(Sum{Turns: 2, Unknown: 2}); got != "unknown tokens in 2 turns (2 turns not reported)" {
		t.Errorf("FormatSum unknown = %q", got)
	}
}

func appendRaw(t *testing.T, dir, s string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, "ledger.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A write waiting for another process's lock must not hold up queries,
// and when it gets the lock it still sees that process's lines first.
func TestWriteWaitDoesNotBlockReads(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir)
	add(t, l, exact("a/1", "a", 10, 1))
	unlock, err := fslock.Lock(filepath.Join(dir, "lock"))
	must(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := l.Add(exact("b/1", "b", 5, 1))
		done <- err
	}()
	// Wait until the writer is in write, waiting for the file lock.
	for l.wmu.TryLock() {
		l.wmu.Unlock()
		runtime.Gosched()
	}
	got := make(chan Sum, 1)
	go func() { got <- l.Total() }()
	select {
	case s := <-got:
		if s.Turns != 1 {
			t.Errorf("total while waiting = %+v", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Total waited for the file lock")
	}
	// Another process writes the same turn while we wait: the waiting
	// writer catches up and doesn't add it again.
	appendRaw(t, dir, `{"v":1,"turn":{"id":"b/1","agent":"b","tokens":{"input":5,"output":1},"status":"exact"}}`+"\n")
	unlock()
	must(t, <-done)
	if s := l.Total(); s.Turns != 2 {
		t.Errorf("total = %+v, want 2 turns", s)
	}
	if s := open(t, dir).Total(); s.Turns != 2 {
		t.Errorf("file's total = %+v, want 2 turns", s)
	}
}

// Writers in one process take turns.
func TestConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			if _, err := l.Add(exact(fmt.Sprintf("a/%d", i%10), "a", 1, 1)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	fresh := open(t, dir)
	if s := fresh.Total(); s.Turns != 10 || fresh.Skipped() != 0 {
		t.Errorf("total = %+v, skipped %d", s, fresh.Skipped())
	}
}

func TestKinds(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir)
	work := exact("a/1", "a", 100, 10)
	work.Kind = KindWork
	decide := exact("p/1", "p", 50, 5)
	decide.Kind = KindDecide
	wake := exact("a/2", "a", 20, 2)
	wake.Kind = KindWake
	for _, turn := range []Turn{work, decide, wake} {
		add(t, l, turn)
	}
	// A line from before kinds, and one from a newer grove.
	appendRaw(t, dir, `{"v":1,"turn":{"id":"old/1","agent":"old","tokens":{"input":7,"output":0},"status":"exact"}}`+"\n")
	appendRaw(t, dir, `{"v":1,"turn":{"id":"new/1","agent":"new","tokens":{"input":3,"output":0},"status":"exact","kind":"later"}}`+"\n")

	b, err := os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kind":"work"`) {
		t.Errorf("kind not written:\n%s", b)
	}
	// Left out when empty.
	add(t, l, exact("x/1", "x", 1, 0))
	b, err = os.ReadFile(filepath.Join(dir, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if last := lines[len(lines)-1]; strings.Contains(last, "kind") {
		t.Errorf("empty kind written: %s", last)
	}

	// Read back by a fresh ledger: old lines count as unknown, kinds from a
	// newer grove come after the known ones.
	var names []string
	for _, k := range open(t, dir).ByKind() {
		names = append(names, string(k.Kind)+"="+Compact(k.Sum.Tokens.Total()))
	}
	want := "work=110 wake=22 decide=55 unknown=8 later=3"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("ByKind = %s, want %s", got, want)
	}
	if l.Skipped() != 0 {
		t.Errorf("skipped %d lines", l.Skipped())
	}
}

func TestShare(t *testing.T) {
	whole := Sum{Tokens: Tokens{Input: 1000}, Turns: 2, Exact: 2}
	part := Sum{Tokens: Tokens{Input: 333}, Turns: 1, Exact: 1}
	if got := Share(part, whole); got != "33%" {
		t.Errorf("Share = %q", got)
	}
	cut := whole
	cut.Exact, cut.Partial = 1, 1
	if got := Share(part, cut); got != "≈33%" {
		t.Errorf("Share of an incomplete sum = %q", got)
	}
	if got := Share(Sum{Turns: 1, Unknown: 1}, whole); got != "?" {
		t.Errorf("Share of nothing reported = %q", got)
	}
	if got := Share(part, Sum{}); got != "?" {
		t.Errorf("Share of no tokens = %q", got)
	}
}
