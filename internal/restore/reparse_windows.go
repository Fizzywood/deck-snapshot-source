//go:build windows

package restore

import (
	"os"
	"syscall"
)

const fileAttributeReparsePoint = 0x400

func isLinkOrReparsePoint(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && attributes.FileAttributes&fileAttributeReparsePoint != 0
}
