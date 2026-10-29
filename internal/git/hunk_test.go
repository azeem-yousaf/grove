package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const twoFiles = `diff --git a/a.txt b/a.txt
index 1..2 100644
--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,3 @@
 one
-two
+TWO
 three
@@ -10,2 +10,3 @@ func x()
 ten
 eleven
+twelve
diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+hello
+world
\ No newline at end of file
`

func TestParseHunks(t *testing.T) {
	hs := ParseHunks(twoFiles)
	if len(hs) != 3 {
		t.Fatalf("got %d hunks: %+v", len(hs), hs)
	}
	a := hs[0]
	if a.Path != "a.txt" || a.OldStart != 1 || a.OldCount != 3 || a.NewStart != 1 || a.NewCount != 3 ||
		strings.Join(a.Old, ",") != "one,two,three" || strings.Join(a.New, ",") != "one,TWO,three" || len(a.Lines) != 4 {
		t.Errorf("first hunk = %+v", a)
	}
	if b := hs[1]; b.NewStart != 10 || len(b.New) != 3 || len(b.Old) != 2 {
		t.Errorf("second hunk = %+v", b)
	}
	if n := hs[2]; n.Path != "new.txt" || !n.NewFile || n.OldCount != 0 || !n.NewNoEOL || n.OldNoEOL || len(n.Lines) != 3 {
		t.Errorf("new file hunk = %+v", n)
	}
}

func TestRevertHunk(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	hunk := func(diff string) Hunk {
		t.Helper()
		hs := ParseHunks(diff)
		if len(hs) != 1 {
			t.Fatalf("want one hunk in:\n%s", diff)
		}
		return hs[0]
	}

	// An LF file.
	write("a.txt", "one\nTWO\nthree\nfour\n")
	edit := hunk("diff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n")
	if err := RevertHunk(dir, edit); err != nil || read("a.txt") != "one\ntwo\nthree\nfour\n" {
		t.Errorf("LF: %v, %q", err, read("a.txt"))
	}
	// Again: the file no longer has the new side.
	if err := RevertHunk(dir, edit); !errors.Is(err, ErrHunkMoved) {
		t.Errorf("second revert: %v", err)
	}

	// A CRLF file, with a diff in git's LF view of it.
	write("a.txt", "one\r\nTWO\r\nthree\r\n")
	if err := RevertHunk(dir, edit); err != nil || read("a.txt") != "one\r\ntwo\r\nthree\r\n" {
		t.Errorf("CRLF: %v, %q", err, read("a.txt"))
	}

	// Lines added at the end of a file that had no final newline.
	write("b.txt", "x\ny\nz\n")
	add := hunk("diff --git a/b.txt b/b.txt\n--- a/b.txt\n+++ b/b.txt\n@@ -1,2 +1,3 @@\n x\n-y\n\\ No newline at end of file\n+y\n+z\n")
	if err := RevertHunk(dir, add); err != nil || read("b.txt") != "x\ny" {
		t.Errorf("no final newline: %v, %q", err, read("b.txt"))
	}

	// Pure insertion: the new side is empty at its start.
	write("c.txt", "a\nb\nc\n")
	ins := hunk("diff --git a/c.txt b/c.txt\n--- a/c.txt\n+++ b/c.txt\n@@ -1,0 +2 @@ \n+b\n")
	if err := RevertHunk(dir, ins); err != nil || read("c.txt") != "a\nc\n" {
		t.Errorf("insertion: %v, %q", err, read("c.txt"))
	}
	removal := hunk("diff --git a/c.txt b/c.txt\n--- a/c.txt\n+++ b/c.txt\n@@ -2 +1,0 @@\n-b\n")
	if err := RevertHunk(dir, removal); err != nil || read("c.txt") != "a\nb\nc\n" {
		t.Errorf("removal: %v, %q", err, read("c.txt"))
	}

	// A new file goes; a deleted one comes back.
	write("new.txt", "hello\nworld")
	nf := ParseHunks(twoFiles)[2]
	if err := RevertHunk(dir, nf); err != nil {
		t.Errorf("new file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("new file still there: %v", err)
	}
	gone := hunk("diff --git a/sub/gone.txt b/sub/gone.txt\ndeleted file mode 100644\n--- a/sub/gone.txt\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-back\n-again\n")
	if err := RevertHunk(dir, gone); err != nil || read("sub/gone.txt") != "back\nagain\n" {
		t.Errorf("deleted file: %v", err)
	}
}

func TestWorktreeDiffAndSince(t *testing.T) {
	r, _ := testRepo(t)
	ctx := context.Background()
	snap, err := Scan(ctx, r, "main", ModeBranch)
	if err != nil {
		t.Fatal(err)
	}
	var a State
	for _, s := range snap.States {
		if s.Branch == "feature/a" {
			a = s
		}
	}
	raw, err := WorktreeDiff(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	hs := ParseHunks(raw)
	var paths []string
	for _, h := range hs {
		paths = append(paths, h.Path)
	}
	// In the scan's order, untracked files included.
	var want []string
	for _, f := range a.Files {
		want = append(want, f.Path)
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("hunk files = %v, want %v", paths, want)
	}

	tree, err := Checkpoint(ctx, a.Path)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := DiffSince(ctx, a.Path, tree); err != nil || d != "" {
		t.Errorf("nothing changed: %q, %v", d, err)
	}
	os.WriteFile(filepath.Join(a.Path, "later.txt"), []byte("later\n"), 0o644)
	d, err := DiffSince(ctx, a.Path, tree)
	if hs := ParseHunks(d); err != nil || len(hs) != 1 || hs[0].Path != "later.txt" || !hs[0].NewFile {
		t.Errorf("since the checkpoint: %v\n%s", err, d)
	}
}
