//go:build windows

package restore

import "os"

// Transactional restore fails closed on Windows before mutation. The
// handle-relative primitive remains available for collision-safe plan/report
// publication without reopening any pathname components.
func syncDirectoryRoot(*os.Root) error { return nil }
