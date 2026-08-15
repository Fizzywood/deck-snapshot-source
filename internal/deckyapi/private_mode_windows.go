//go:build windows

package deckyapi

import "os"

func privateArchiveMode(os.FileInfo) bool { return true }

func trustedVersionFile(os.FileInfo) bool { return true }
