// Package store keeps grove's own state for a repository, such as agent
// conversations and plans, as JSON files under <git common dir>/grove. Git
// never commits anything in there, and every worktree shares it.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store is one repository's state folder.
type Store struct {
	dir string
}

// Open returns the store for the repository whose common git directory is
// commonDir. Nothing is created until something is saved.
func Open(commonDir string) *Store {
	return &Store{dir: filepath.Join(commonDir, "grove")}
}

// Dir is the folder the store writes to.
func (s *Store) Dir() string { return s.dir }

// path turns a slash-separated name such as "sessions/abc.json" into a file
// path, refusing names that would leave the store.
func (s *Store) path(name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	// Rooted names count as absolute everywhere: on Windows /x has no drive
	// but would still leave the store, and C:x is relative to C:'s folder.
	rooted := strings.HasPrefix(name, "/") || strings.HasPrefix(name, `\`) || strings.Contains(name, ":")
	if name == "" || rooted || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("store: bad name " + name)
	}
	return filepath.Join(s.dir, clean), nil
}

// Save writes v as JSON to name. The file is replaced in one step, so a
// crash or a second grove never leaves half a file behind.
func (s *Store) Save(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return s.Write(name, b)
}

// Write stores b as name, replacing it in one step.
func (s *Store) Write(name string, b []byte) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// Load reads name into v. A missing file returns an error that satisfies
// errors.Is(err, os.ErrNotExist).
func (s *Store) Load(name string, v any) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Remove deletes name. Removing something that isn't there is not an error.
func (s *Store) Remove(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List returns the names of the .json files in folder, sorted, as names
// Load accepts. A missing folder is empty.
func (s *Store) List(folder string) ([]string, error) {
	p, err := s.path(folder)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, folder+"/"+e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
