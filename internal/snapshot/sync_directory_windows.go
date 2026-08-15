//go:build windows

package snapshot

import "os"

// NTFS journals the handle-relative rename. Transactional restore remains
// disabled on Windows because directory metadata cannot be durably flushed
// through the read-only os.Root handle.
func syncPublishedDirectory(*os.Root) error { return nil }
