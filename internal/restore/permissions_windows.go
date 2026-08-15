//go:build windows

package restore

import "os"

func platformDirectoryWritable(string) error { return nil }

func platformOwnedByCurrentUser(os.FileInfo) error { return nil }

func platformDeckyManagedOwner(os.FileInfo) error { return nil }

func platformDeckyManagedMode(os.FileInfo) error { return nil }
