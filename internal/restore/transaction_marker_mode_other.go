//go:build !linux

package restore

import "io/fs"

// Windows ACLs do not round-trip Unix permission bits. The held secure parent
// and regular-file checks remain meaningful there; transactional restore itself
// is still Linux-only.
func privateMarkerMode(info fs.FileInfo) bool { return info.Mode().IsRegular() }
