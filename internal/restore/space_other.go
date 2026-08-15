//go:build !linux && !windows

package restore

import "errors"

func AvailableBytes(string) (uint64, error) {
	return 0, errors.New("free-space detection is not implemented on this platform")
}
