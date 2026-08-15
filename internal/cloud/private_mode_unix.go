//go:build !windows

package cloud

import (
	"errors"
	"os"
)

func privateFileModeError(info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("cloud configuration permissions are too broad")
	}
	return nil
}
