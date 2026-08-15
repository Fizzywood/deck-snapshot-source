//go:build !windows

package restore

func appliedModeMatches(actual, expected uint32) bool {
	return actual == expected
}
