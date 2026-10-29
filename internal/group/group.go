// Package group is the chat shared by a plan's grouped agents and the user.
// A group lives in a folder of grove's store: messages.jsonl, one message
// per line, members.json, written by grove, and a read cursor per member.
// Every agent's tool server and grove itself read and write the same files,
// so nothing has to stay running between them.
package group

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// User is the From of messages the user posts.
const User = "you"

// Grove is the From of messages grove itself posts, such as a warning that
// a member's worktree now conflicts with another.
const Grove = "grove"

// Planner is the From of messages the planner posts while it watches a plan.
const Planner = "planner"

// Message is one post to a group.
type Message struct {
	Time time.Time `json:"time"`
	From string    `json:"from"`         // a member's name, User or Grove
	To   string    `json:"to,omitempty"` // one member, or "" for everyone
	Text string    `json:"text"`
}

// For reports whether member should read m: it is addressed to them or to
// everyone, and they didn't write it.
func (m Message) For(member string) bool {
	return m.From != member && (m.To == "" || m.To == member)
}

// Member is one agent in a group.
type Member struct {
	Name   string `json:"name"` // its branch, which is unique
	Task   string `json:"task"`
	Brief  string `json:"brief,omitempty"`
	Path   string `json:"path,omitempty"` // its worktree, once created
	Status string `json:"status,omitempty"`
}

// Info describes a group for its members.
type Info struct {
	Plan    string   `json:"plan"`
	Name    string   `json:"name"`
	Members []Member `json:"members"`
}

// Log is one group's folder.
type Log struct {
	Dir string
}

// Dir is where a plan's group is kept under the store folder.
func Dir(storeDir, planID, groupID string) string {
	return filepath.Join(storeDir, "groups", planID+"-"+groupID)
}

func (l Log) messages() string { return filepath.Join(l.Dir, "messages.jsonl") }

// cursorFile holds how many messages member has read.
func (l Log) cursorFile(member string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r < ' ' {
			return '_'
		}
		return r
	}, member)
	return filepath.Join(l.Dir, "read-"+safe)
}

// lock serialises writers across processes with a lock file. A lock older
// than staleLock was left by a process that died, and is taken over.
func (l Log) lock() (func(), error) {
	p := filepath.Join(l.Dir, "lock")
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(p) }, nil
		}
		// Windows answers "access denied" while another writer's lock is
		// being deleted, so that is a busy lock too until the deadline.
		denied := runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission)
		if !errors.Is(err, os.ErrExist) && !denied {
			return nil, err
		}
		if fi, serr := os.Stat(p); serr == nil && time.Since(fi.ModTime()) > staleLock {
			os.Remove(p)
			continue
		}
		if time.Now().After(deadline) {
			if denied {
				return nil, err
			}
			return nil, errors.New("the group chat is locked by another program")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const staleLock = 10 * time.Second

// Post appends a message.
func (l Log) Post(m Message) error {
	m.Text = strings.TrimSpace(m.Text)
	if m.Text == "" {
		return errors.New("the message is empty")
	}
	if m.Time.IsZero() {
		m.Time = time.Now()
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return err
	}
	unlock, err := l.lock()
	if err != nil {
		return err
	}
	defer unlock()
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.messages(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// All reads every message. A line cut short by a crash is skipped.
func (l Log) All() ([]Message, error) {
	b, err := os.ReadFile(l.messages())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Message
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Text != "" {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

// Cursor is how many messages member has read.
func (l Log) Cursor(member string) int {
	b, err := os.ReadFile(l.cursorFile(member))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func (l Log) setCursor(member string, n int) error {
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(l.cursorFile(member), []byte(strconv.Itoa(n)), 0o644)
}

// Unread returns the messages for member that arrived since it last read,
// without marking them read.
func (l Log) Unread(member string) ([]Message, error) {
	all, err := l.All()
	if err != nil {
		return nil, err
	}
	var out []Message
	for _, m := range all[min(l.Cursor(member), len(all)):] {
		if m.For(member) {
			out = append(out, m)
		}
	}
	return out, nil
}

// Read returns member's unread messages and marks everything so far read.
// With wait > 0 and nothing new, it waits up to that long for a message.
func (l Log) Read(member string, wait time.Duration) ([]Message, error) {
	deadline := time.Now().Add(wait)
	for {
		all, err := l.All()
		if err != nil {
			return nil, err
		}
		var out []Message
		for _, m := range all[min(l.Cursor(member), len(all)):] {
			if m.For(member) {
				out = append(out, m)
			}
		}
		if len(out) > 0 || !time.Now().Before(deadline) {
			return out, l.setCursor(member, len(all))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// WriteInfo saves who is in the group, for its members to look up.
func (l Log) WriteInfo(info Info) error {
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(l.Dir, ".members.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(l.Dir, "members.json"))
}

// ReadInfo loads what WriteInfo saved.
func (l Log) ReadInfo() (Info, error) {
	var info Info
	b, err := os.ReadFile(filepath.Join(l.Dir, "members.json"))
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(b, &info)
}

// Format renders messages as plain text for an agent.
func Format(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		to := ""
		if m.To != "" {
			to = " (to " + m.To + ")"
		}
		fmt.Fprintf(&b, "[%s] %s%s: %s\n", m.Time.Format("15:04"), m.From, to, m.Text)
	}
	return b.String()
}
