// Package fslock serialises writers to grove's shared files across
// processes: grove itself and the tool servers agents start.
package fslock

import (
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
		// being deleted, so that is a busy lock too until the
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

// release removes the lock file if it is still this hold's. It does so
// under the break lock, so a waiter breaking the lock as stale can't remove
// a lock created between its check and its removal.
func release(path, token string) {
	if unbreak := breakLock(path, time.Second); unbreak != nil {
		defer unbreak()
	}
	if b, err := os.ReadFile(path); err == nil && string(b) == token {
		os.Remove(path)
	}
}

// takeOver removes a stale lock, reporting whether it did. Removing a lock
// file, here or in release, only happens under the break lock, and a new
// lock can only be created once the file is gone, so the lock checked as
// stale is the one removed. Of several waiters that saw the same stale lock
// the first removes it and the others find it fresh, or gone.
func takeOver(path string) bool {
	if fi, err := os.Stat(path); err != nil || time.Since(fi.ModTime()) <= Stale {
		return false
	}
	unbreak := breakLock(path, 0)
	if unbreak == nil {
		return false // another waiter is breaking it
	}
	defer unbreak()
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) <= Stale {
		return false
	}
	return os.Remove(path) == nil
}

// breakStale is how old a break lock is before it counts as left by a
// process that died while holding it; it is only ever held for a moment.
const breakStale = 2 * time.Second

// breakLock takes the short-lived lock that guards removing path's lock
// file, waiting up to wait for it. It returns nil if it couldn't.
func breakLock(path string, wait time.Duration) func() {
	bpath := path + ".break"
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(bpath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(bpath) }
		}
		denied := runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission)
		if !errors.Is(err, os.ErrExist) && !denied {
			return nil
		}
		if fi, err := os.Stat(bpath); err == nil && time.Since(fi.ModTime()) > breakStale {
			os.Remove(bpath)
			continue
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}
