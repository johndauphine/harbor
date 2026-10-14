//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
)

func lockStoreFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errStoreInUse
	}
	if err != nil {
		return storeLockError(err)
	}
	return nil
}

func validateStoreLinks(f *os.File) error {
	var info unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &info); err != nil {
		return err
	}
	if info.Nlink != 1 {
		return errors.New("Hard-linked configuration databases are not supported; use one database path or a symbolic link")
	}
	return nil
}

func canonicalStorePath(_ *os.File, path string) (string, error) { return filepath.EvalSymlinks(path) }
