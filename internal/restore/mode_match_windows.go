//go:build windows

package restore

// Windows does not preserve the POSIX permission bits stored in a snapshot.
// File identity is still verified by size and digest on this platform.
func appliedModeMatches(_, _ uint32) bool {
	return true
}
