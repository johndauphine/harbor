package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var errStoreInUse = errors.New("This Harbor configuration is already open in another process; close that instance before starting Harbor again")

type storeLock struct {
	file *os.File
	path string
}

// acquireStoreLock runs before opening SQLite or accessing the vault. Lock the
// canonical-path sidecar so aliases cannot bypass it. Hard-linked databases are
// rejected: they can have multiple canonical paths. The sidecar is never deleted.
// The OS releases the lock on process exit, including an abrupt termination.
func acquireStoreLock(path string) (*storeLock, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".harbor", "profiles.db")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*storeLock, error) { f.Close(); return nil, err }
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(errors.New("Configuration database must be a regular file"))
	}
	if err = validateStoreLinks(f); err != nil {
		return fail(err)
	}
	canonical, err := canonicalStorePath(f, path)
	if err != nil {
		return fail(err)
	}
	// A path replacement during acquisition must not leave us locking one file
	// while SQLite opens another. Application data directories must remain trusted.
	current, err := os.Stat(canonical)
	if err != nil {
		return fail(err)
	}
	if !os.SameFile(info, current) {
		return fail(errors.New("Configuration database changed while acquiring its lock; try again"))
	}
	f.Close()
	lockFile, err := os.OpenFile(canonical+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockStoreFile(lockFile); err != nil {
		lockFile.Close()
		return nil, err
	}
	current, err = os.Stat(canonical)
	if err != nil || !os.SameFile(info, current) {
		lockFile.Close()
		return nil, errors.New("Configuration database changed while acquiring its lock; try again")
	}
	return &storeLock{file: lockFile, path: canonical}, nil
}
func (l *storeLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
func storeLockError(err error) error {
	return fmt.Errorf("Unable to lock Harbor configuration: %w", err)
}
