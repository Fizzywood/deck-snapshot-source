//go:build linux

package restore

import "io/fs"

func privateMarkerMode(info fs.FileInfo) bool { return info.Mode().Perm() == 0o600 }
