//go:build !windows

package snapshot

import "os"

func samePublishedFile(before, after os.FileInfo) bool { return os.SameFile(before, after) }
