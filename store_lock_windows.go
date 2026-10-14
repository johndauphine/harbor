package main

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"strings"
)

func lockStoreFile(f *os.File) error {
	offset := windows.Overlapped{}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &offset)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errStoreInUse
	}
	if err != nil {
		return storeLockError(err)
	}
	return nil
}

func validateStoreLinks(f *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.NumberOfLinks != 1 {
		return errors.New("Hard-linked configuration databases are not supported; use one database path or a symbolic link")
	}
	return nil
}

// Resolve through the open handle to normalize junctions and short (8.3) names
// as well as ordinary symlinks. Case aliases address the same sidecar on NTFS.
func canonicalStorePath(f *os.File, _ string) (string, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			path := windows.UTF16ToString(buf[:n])
			// The SQLite driver treats '?' as a DSN query delimiter, so pass
			// its equivalent ordinary DOS/UNC path rather than the device prefix.
			if strings.HasPrefix(path, `\\?\UNC\`) {
				return `\\` + strings.TrimPrefix(path, `\\?\UNC\`), nil
			}
			return strings.TrimPrefix(path, `\\?\`), nil
		}
		buf = make([]uint16, n+1)
	}
}
