//go:build !linux

package deckyapi

import "os"

func heldArchivePath(_ *os.File, original string) string { return original }
