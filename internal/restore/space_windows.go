//go:build windows

package restore

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceExProc = kernel32.NewProc("GetDiskFreeSpaceExW")
)

// AvailableBytes returns free bytes available to the current user on the
// filesystem containing path.
func AvailableBytes(path string) (uint64, error) {
	pointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available uint64
	result, _, callErr := getDiskFreeSpaceExProc.Call(
		uintptr(unsafe.Pointer(pointer)),
		uintptr(unsafe.Pointer(&available)),
		0,
		0,
	)
	if result == 0 {
		return 0, fmt.Errorf("GetDiskFreeSpaceExW: %w", callErr)
	}
	return available, nil
}
