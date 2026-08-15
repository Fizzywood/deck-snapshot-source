//go:build windows

package snapshot

import "os"

// Windows os.Root metadata does not currently expose a stable SameFile identity
// after the handle-relative NT rename. Cloud transfer execution remains Linux-only.
func samePublishedFile(before, after os.FileInfo) bool {
	return before.Mode().IsRegular() && after.Mode().IsRegular() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}
