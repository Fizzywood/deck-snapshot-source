//go:build !linux && !windows

package restore

import (
	"errors"
	"os"
)

func renameNoReplaceRoots(*os.Root, string, *os.Root, string) error {
	return errors.New("atomic no-replace moves are unsupported on this platform")
}
