package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/azeem-yousaf/grove/internal/agent"
)

const reviewDiff = `diff --git a/a.txt b/a.txt
--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three
diff --git a/b.txt b/b.txt
new file mode 100644
--- /dev/null
+++ b/b.txt
@@ -0,0 +1,2 @@
+hello
+hello again
`

// openReview opens the whole-worktree view on the selected worktree and
// answers its load with raw.
func openReview(t *testing.T, m *Model, raw string) {
	t.Helper()
	press(m, "v")
	if m.wtView == nil || m.focus != focusDiff {
		t.Fatalf("v didn't open the worktree view: view=%v focus=%v", m.wtView != nil, m.focus)
	}
	m.Update(wtDiffMsg{key: m.wtView.loading, raw: raw})
}

func TestWorktreeView(t *testing.T) {
	m := fakeModel(t, 140, 40)
	openReview(t, m, reviewDiff)
	out := plain(m)
	for _, want := range []string{"all changes · 2 files", "▍a.txt", "▍b.txt", "+ TWO", "+ hello again", "x put back"} {
		if !strings.Contains(out, want) {
			t.Errorf("view is missing %q:\n%s", want, out)
		}
	}
	// ] goes from file to file, n and N from hunk to hunk, and a bar in
	// the gutter shows the current one.
	if h, _, _ := m.currentHunk(); h.Path != "a.txt" {
		t.Errorf("current hunk at the start: %q", h.Path)
	}
	press(m, "]")
	if h, _, _ := m.currentHunk(); h.Path != "b.txt" {
		t.Errorf("] went to %q", h.Path)
	}
	press(m, "N")
	if h, _, _ := m.currentHunk(); h.Path != "a.txt" {
		t.Errorf("N went to %q", h.Path)
	}
	if bars := strings.Count(plain(m), "▌"); bars != 4 {
		t.Errorf("want a bar beside the 4 lines of a.txt's hunk, got %d:\n%s", bars, plain(m))
	}

	// No turn yet: v can't switch to the last turn.
	press(m, "v")
	if m.wtView.since || !strings.Contains(m.toast.text, "hasn't had a turn") {
		t.Errorf("v without a turn: since=%v toast=%q", m.wtView.since, m.toast.text)
	}
	s := m.newSession(m.wtView.path, "feature/auth", agent.Claude)
	s.turnTree = "abc"
	press(m, "v")
	if !m.wtView.since || !strings.Contains(m.wtView.loading, "since|") {
		t.Fatalf("v with a turn: %+v", m.wtView)
	}
	m.Update(wtDiffMsg{key: m.wtView.loading, raw: ""})
	if out := plain(m); !strings.Contains(out, "Nothing changed since the agent's last turn began") {
		t.Errorf("empty last turn:\n%s", out)
	}

	// Back to the list closes it.
	press(m, "tab")
	if m.wtView != nil {
		t.Error("the view stayed open in the list")
	}
}

func TestDiffSearch(t *testing.T) {
	m := fakeModel(t, 140, 40)
	openReview(t, m, reviewDiff)
	press(m, "/")
	typeText(m, "HELLO")
	if m.searchQuery != "hello" || m.parsed.lines[m.searchAt].text != "hello" {
		t.Fatalf("search: query=%q at=%d", m.searchQuery, m.searchAt)
	}
	if out := plain(m); !strings.Contains(out, "1 of 2") {
		t.Errorf("footer doesn't count matches:\n%s", out)
	}
	press(m, "enter", "n")
	if m.searchActive || m.parsed.lines[m.searchAt].text != "hello again" {
		t.Errorf("n after enter: active=%v at=%q", m.searchActive, m.parsed.lines[m.searchAt].text)
	}
	press(m, "n") // wraps around
	if m.parsed.lines[m.searchAt].text != "hello" {
		t.Errorf("n didn't wrap: %q", m.parsed.lines[m.searchAt].text)
	}
	press(m, "esc")
	if m.searchQuery != "" || m.focus != focusDiff {
		t.Errorf("first esc should only clear the search: query=%q focus=%v", m.searchQuery, m.focus)
	}
	press(m, "esc")
	if m.focus != focusList {
		t.Error("second esc should go back to the list")
	}
}

func TestRevertFromView(t *testing.T) {
	m := fakeModel(t, 140, 40)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\r\nTWO\r\nthree\r\n"), 0o644)
	m.snap.States[1].Path = dir
	m.rebuildRows()
	openReview(t, m, reviewDiff)

	press(m, "x")
	if m.revert == nil {
		t.Fatal("x didn't ask")
	}
	out := plain(m)
	for _, want := range []string{"Put this change back", "a.txt", "lines 1–3", "+TWO", "-two"} {
		if !strings.Contains(out, want) {
			t.Errorf("revert dialog is missing %q:\n%s", want, out)
		}
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.revert != nil || cmd == nil {
		t.Fatal("enter didn't put it back")
	}
	m.Update(cmd())
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one\r\ntwo\r\nthree\r\n" {
		t.Errorf("a.txt = %q", b)
	}
	if !strings.Contains(m.toast.text, "Put back") {
		t.Errorf("toast = %q", m.toast.text)
	}

	// Doing it again finds the file changed and says so.
	m.wtView.loaded = ""
	m.Update(wtDiffMsg{key: m.wtView.loading, raw: reviewDiff})
	press(m, "x")
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m.Update(cmd())
	if !strings.Contains(m.toast.text, "changed since") {
		t.Errorf("second revert: toast = %q", m.toast.text)
	}
}

func TestHunkToAgent(t *testing.T) {
	m := fakeModel(t, 140, 40)
	m.installed = func() []agent.Kind { return []agent.Kind{agent.Claude} }
	openReview(t, m, reviewDiff)
	press(m, "]", ">")
	if m.focus != focusChat {
		t.Fatalf("> didn't open the chat: focus=%v", m.focus)
	}
	draft := m.chatInput.Value()
	for _, want := range []string{"About this change to b.txt, from line 1", "```diff", "+hello again"} {
		if !strings.Contains(draft, want) {
			t.Errorf("draft is missing %q:\n%s", want, draft)
		}
	}
	if strings.Contains(draft, "TWO") {
		t.Errorf("draft quotes the wrong hunk:\n%s", draft)
	}
}
