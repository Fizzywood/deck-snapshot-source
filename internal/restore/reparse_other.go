//go:build !windows

package restore

import "os"

func isLinkOrReparsePoint(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}
