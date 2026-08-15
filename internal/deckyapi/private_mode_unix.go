//go:build unix

package deckyapi

import (
	"os"
	"syscall"
)

func privateArchiveMode(info os.FileInfo) bool { return info.Mode().Perm()&0o077 == 0 }

func trustedVersionFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (int(stat.Uid) == os.Geteuid() || stat.Uid == 0) && info.Mode().Perm()&0o022 == 0
}
