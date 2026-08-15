//go:build !linux

package restore

import (
	"errors"
	"os"
)

func exchangeRoots(*os.Root, string, *os.Root, string) error {
	return errors.New("atomic file exchange is unsupported on this platform")
}
