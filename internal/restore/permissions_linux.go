//go:build linux

package restore

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func platformDirectoryWritable(path string) error {
	if err := unix.Access(path, unix.W_OK|unix.X_OK); err != nil {
		return errors.New("directory is not writable and searchable by the current user")
	}
	return nil
}

func platformOwnedByCurrentUser(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("file is not owned by the current user")
	}
	return nil
}

func platformDeckyManagedOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(stat.Uid) != os.Geteuid() && stat.Uid != 0) {
		return errors.New("file is not owned by Decky Loader or the current user")
	}
	return nil
}

func platformDeckyManagedMode(info os.FileInfo) error {
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("file is group- or world-writable")
	}
	return nil
}
