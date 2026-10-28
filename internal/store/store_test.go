package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	common := t.TempDir()
	s := Open(common)
	if s.Dir() != filepath.Join(common, "grove") {
		t.Errorf("Dir = %s", s.Dir())
	}

	type thing struct{ Name string }
	var got thing
	if err := s.Load("sessions/a.json", &got); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	if names, err := s.List("sessions"); err != nil || len(names) != 0 {
		t.Errorf("missing folder lists %v, %v", names, err)
	}
	if err := s.Save("sessions/b.json", thing{"b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("sessions/a.json", thing{"a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("sessions/a.json", thing{"a2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Load("sessions/a.json", &got); err != nil || got.Name != "a2" {
		t.Errorf("Load = %+v, %v", got, err)
	}
	names, err := s.List("sessions")
	if err != nil || len(names) != 2 || names[0] != "sessions/a.json" {
		t.Errorf("List = %v, %v", names, err)
	}
	// Replacing a file leaves no temporary files behind.
	entries, _ := os.ReadDir(filepath.Join(s.Dir(), "sessions"))
	if len(entries) != 2 {
		t.Errorf("folder has %d entries, want 2", len(entries))
	}
	if err := s.Remove("sessions/a.json"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("sessions/a.json"); err != nil {
		t.Errorf("removing twice: %v", err)
	}

	for _, bad := range []string{"", "../x.json", "/etc/x.json", "sessions/../../x.json"} {
		if err := s.Save(bad, thing{}); err == nil {
			t.Errorf("Save(%q) should be refused", bad)
		}
	}
}
