//go:build linux

package restore

import "syscall"

// AvailableBytes returns free bytes available to an unprivileged user on the
// filesystem containing path.
func AvailableBytes(path string) (uint64, error) {
	var state syscall.Statfs_t
	if err := syscall.Statfs(path, &state); err != nil {
		return 0, err
	}
	return state.Bavail * uint64(state.Bsize), nil
}
