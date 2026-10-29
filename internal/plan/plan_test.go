package plan

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/azeem-yousaf/grove/internal/store"
)

const reply = "Here's how I'd split it.\n\n" +
	"```grove-plan\n" + `{
  "title": "Guest checkout",
  "groups": [{"id": "checkout", "name": "Checkout flow", "icon": "🛒"}, {"id": "lonely", "name": "Nobody"}],
  "tasks": [
    {"id": "api", "title": "Checkout API", "brief": "Add POST /checkout/guest.", "branch": "feature/guest-api", "group": "checkout"},
    {"id": "ui", "title": "Checkout page", "brief": "Add the guest form.", "branch": "feature/guest-ui", "group": "checkout"},
    {"title": "Docs", "brief": "Document guest checkout.", "branch": "docs/guest", "group": "lonely"}
  ]
}` + "\n```\n\nShall I start?"

func TestFromReply(t *testing.T) {
	now := time.Date(2026, 10, 28, 15, 30, 0, 0, time.UTC)
	p, err := FromReply(reply, "Add guest checkout", now)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Guest checkout" || p.Request != "Add guest checkout" || p.Status != Draft || !strings.HasPrefix(p.ID, "20261028-153000-") {
		t.Errorf("plan = %+v", p)
	}
	if len(p.Tasks) != 3 || p.Tasks[2].ID != "t3" || p.Tasks[0].Status != Proposed {
		t.Fatalf("tasks = %+v", p.Tasks)
	}
	// A group of one has nobody to talk to, so its task becomes isolated.
	if len(p.Groups) != 1 || p.Groups[0].ID != "checkout" || p.Groups[0].Icon != "🛒" || p.Tasks[2].Group != "" {
		t.Errorf("groups = %+v, docs group %q", p.Groups, p.Tasks[2].Group)
	}
	if m := p.Members("checkout"); len(m) != 2 || m[1].ID != "ui" {
		t.Errorf("members = %+v", m)
	}
	if probs := p.Validate(nil); len(probs) != 0 {
		t.Errorf("problems in a good plan: %v", probs)
	}

	if _, err := FromReply("no plan here", "", now); !errors.Is(err, ErrNoPlan) {
		t.Errorf("err = %v, want ErrNoPlan", err)
	}
	if _, err := FromReply("```grove-plan\n{nope\n```", "", now); err == nil {
		t.Error("broken JSON should fail")
	}
}

func TestValidate(t *testing.T) {
	p := &Plan{Tasks: []Task{
		{ID: "a", Title: "A", Brief: "do a", Branch: "feature/a"},
		{ID: "a", Title: "B", Brief: "", Branch: "feature/A"},
		{ID: "c", Title: "", Brief: "do c", Branch: "bad..name"},
		{ID: "d", Title: "D", Brief: "do d", Branch: "main"},
		{ID: "e", Title: "E", Brief: "do e", Branch: ""},
	}}
	got := strings.Join(p.Validate([]string{"main"}), "\n")
	for _, want := range []string{
		`B: another task has the ID "a".`,
		"B: the brief is empty.",
		"B: another task also uses the branch feature/A.",
		"Task 3 has no title.",
		`Task 3: "bad..name" isn't a usable branch name.`,
		"D: the branch main already exists.",
		"E: no branch name.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if probs := (&Plan{}).Validate(nil); len(probs) != 1 {
		t.Errorf("an empty plan should have one problem, got %v", probs)
	}
}

func TestSaveLoadList(t *testing.T) {
	s := store.Open(t.TempDir())
	if plans, err := List(s); err != nil || len(plans) != 0 {
		t.Fatalf("empty store: %v, %v", plans, err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	older, _ := FromReply(reply, "first", t0)
	newer, _ := FromReply(reply, "second", t0.Add(time.Hour))
	for _, p := range []*Plan{older, newer} {
		if err := Save(s, p, p.Created); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Load(s, older.ID)
	if err != nil || got.Request != "first" || len(got.Tasks) != 3 || got.Tasks[0].Group != "checkout" {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	plans, err := List(s)
	if err != nil || len(plans) != 2 || plans[0].Request != "second" {
		t.Fatalf("List = %v, %v; want newest first", plans, err)
	}
	if err := Delete(s, older.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s, older.ID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("deleted plan still loads: %v", err)
	}
}

func TestFromReplyReadsTheTestCommand(t *testing.T) {
	p, err := FromReply("```grove-plan\n{\"title\": \"Shop\", \"test\": \" npm test \", \"tasks\": [{\"title\": \"A\", \"branch\": \"feature/a\", \"brief\": \"b\"}]}\n```", "req", time.Now())
	if err != nil || p.Test != "npm test" {
		t.Fatalf("test = %q, %v", p.Test, err)
	}
}
