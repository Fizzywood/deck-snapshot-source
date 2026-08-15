//go:build linux

package snapshot

import (
	"os"

	"golang.org/x/sys/unix"
)

func publishNoReplace(root *os.Root, oldName, newName string) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return unix.Renameat2(int(directory.Fd()), oldName, int(directory.Fd()), newName, unix.RENAME_NOREPLACE)
}
