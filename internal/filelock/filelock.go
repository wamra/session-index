// Package filelock takes a non-blocking exclusive lock on a file: flock on
// Unix, LockFileEx on Windows. The lock dies with the process.
package filelock

import (
	"errors"
	"os"
)

// ErrLocked is returned by Try when another process holds the lock.
var ErrLocked = errors.New("locked by another process")

// Lock is a held lock.
type Lock struct{ f *os.File }

// Try creates path if needed and locks it without blocking.
func Try(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lock(f); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	unlock(l.f)
	l.f.Close()
}

// File returns the locked file, for a holder that keeps state in it.
func (l *Lock) File() *os.File { return l.f }
