// Package fslock serialises writers to grove's shared files across
// processes: grove itself and the tool servers agents start.
package fslock

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"runtime"
	"time"
)

// Stale is how old a lock file is before it counts as left by a process
// that died, and is taken over.
const Stale = 10 * time.Second

// ErrBusy is returned when another process holds the lock for too long.
var ErrBusy = errors.New("locked by another program")

// Lock creates the lock file path, waiting up to 5 seconds for another
// holder, and returns the function that releases it. The file holds a token
// unique to this hold, so releasing a lock that was taken over as stale
// leaves the new holder's alone.
func Lock(path string) (func(), error) {
	token := newToken()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, werr := f.WriteString(token)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				os.Remove(path)
				return nil, werr
			}
			return func() { release(path, token) }, nil
		}
		// Windows answers "access denied" while another writer's lock is
		// being deleted or renamed, so that is a busy lock too until the
		// deadline.
		denied := runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission)
		if !errors.Is(err, os.ErrExist) && !denied {
			return nil, err
		}
		if takeOver(path) {
			continue
		}
		if time.Now().After(deadline) {
			if denied {
				return nil, err
			}
			return nil, ErrBusy
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// release removes the lock file if it is still this hold's.
func release(path, token string) {
	if b, err := os.ReadFile(path); err == nil && string(b) == token {
		os.Remove(path)
	}
}

// takeOver moves a stale lock out of the way, reporting whether it did.
// Renaming is atomic, so of several waiters that saw the same stale lock
// only one moves it. One that moved a lock created after its check (another
// waiter took the stale one over first) puts that lock back.
func takeOver(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) <= Stale {
		return false
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	aside := path + ".stale-" + newToken()
	if os.Rename(path, aside) != nil {
		return false // gone already, or another waiter is moving it
	}
	moved, rerr := os.ReadFile(aside)
	mfi, serr := os.Stat(aside)
	if rerr == nil && serr == nil && bytes.Equal(moved, old) && time.Since(mfi.ModTime()) > Stale {
		os.Remove(aside)
		return true
	}
	// A live lock: back where it was, unless someone holds a newer one by
	// now. Link never replaces a file; rename is the fallback where links
	// aren't supported.
	if err := os.Link(aside, path); err != nil && !errors.Is(err, os.ErrExist) {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if os.Rename(aside, path) == nil {
				return false
			}
		}
	}
	os.Remove(aside)
	return false
}
