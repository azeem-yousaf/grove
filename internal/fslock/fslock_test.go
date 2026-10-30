package fslock

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makeStale(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * Stale)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// A holder slower than Stale loses the lock; releasing it afterwards must
// leave the new holder's lock alone.
func TestStolenLockReleaseKeepsNewHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock1, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	makeStale(t, path)
	unlock2, err := Lock(path)
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	unlock1()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the old holder removed the new holder's lock: %v", err)
	}
	unlock2()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lock left after release: %v", err)
	}
	if left, _ := filepath.Glob(path + ".stale-*"); len(left) > 0 {
		t.Errorf("leftovers: %v", left)
	}
}

// Waiters that all find the same stale lock take turns: at most one holds
// it at a time.
func TestStaleTakeoverExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(path, []byte("dead"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeStale(t, path)
	var in, most atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			unlock, err := Lock(path)
			if err != nil {
				t.Error(err)
				return
			}
			n := in.Add(1)
			for {
				m := most.Load()
				if n <= m || most.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			in.Add(-1)
			unlock()
		})
	}
	wg.Wait()
	if most.Load() != 1 {
		t.Errorf("%d holders at once", most.Load())
	}
}

// Moving a lock that turned out to be live puts it back.
func TestTakeOverLeavesLiveLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if takeOver(path) {
		t.Fatal("a fresh lock was taken over")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
