//go:build linux

package deckyapi

import (
	"fmt"
	"os"
)

func heldArchivePath(file *os.File, _ string) string {
	return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), file.Fd())
}
