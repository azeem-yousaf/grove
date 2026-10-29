// Package fslock serialises writers to grove's shared files across
// processes: grove itself and the tool servers agents start.
package fslock

import (
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
// holder, and returns the function that releases it.
func Lock(path string) (func(), error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		// Windows answers "access denied" while another writer's lock is
		// being deleted, so that is a busy lock too until the deadline.
		denied := runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission)
		if !errors.Is(err, os.ErrExist) && !denied {
			return nil, err
		}
		if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > Stale {
			os.Remove(path)
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
