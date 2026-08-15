//go:build linux

package restore

import (
	"os"

	"golang.org/x/sys/unix"
)

func exchangeRoots(firstRoot *os.Root, firstName string, secondRoot *os.Root, secondName string) error {
	firstDirectory, err := firstRoot.Open(".")
	if err != nil {
		return err
	}
	defer firstDirectory.Close()
	secondDirectory, err := secondRoot.Open(".")
	if err != nil {
		return err
	}
	defer secondDirectory.Close()
	return unix.Renameat2(int(firstDirectory.Fd()), firstName, int(secondDirectory.Fd()), secondName, unix.RENAME_EXCHANGE)
}
