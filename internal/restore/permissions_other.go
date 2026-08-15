//go:build !linux && !windows

package restore

import (
	"errors"
	"os"
)

func platformDirectoryWritable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o200 == 0 {
		return errors.New("directory is not owner-writable")
	}
	return nil
}

func platformOwnedByCurrentUser(os.FileInfo) error { return nil }

func platformDeckyManagedOwner(os.FileInfo) error { return nil }

func platformDeckyManagedMode(os.FileInfo) error { return nil }
