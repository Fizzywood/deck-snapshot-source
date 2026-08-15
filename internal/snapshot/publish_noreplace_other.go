//go:build !linux && !windows

package snapshot

import (
	"errors"
	"os"
)

func publishNoReplace(*os.Root, string, string) error {
	return errors.New("atomic no-replace snapshot publication is unsupported on this platform")
}
