//go:build !windows

package cloud

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateCloudDirectoriesRejectSymlinkComponents(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "redirect")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(filepath.Join(link, "cloud")); err == nil {
		t.Fatal("ensurePrivateDirectory() followed a symlink component")
	}
	if _, err := os.Lstat(filepath.Join(outside, "cloud")); !os.IsNotExist(err) {
		t.Fatalf("unsafe directory was created outside the intended root: %v", err)
	}
	material, err := GenerateRecovery(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRecovery(filepath.Join(link, "recovery.json"), material); err == nil {
		t.Fatal("SaveRecovery() accepted a symlinked export directory")
	}
}

func TestPrivateCloudDirectoryCreationIsComponentWise(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state", "deck-snapshot", "cloud")
	if err := ensurePrivateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory mode = %v, %v", info, err)
	}
}
