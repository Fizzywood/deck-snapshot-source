//go:build linux

package deckyapi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHeldArchivePathRemainsBoundToOpenedInode(t *testing.T) {
	directory := t.TempDir()
	original := filepath.Join(directory, "package.zip")
	if err := os.WriteFile(original, []byte("approved"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	held := heldArchivePath(file, original)
	displaced := filepath.Join(directory, "displaced.zip")
	if err := os.Rename(original, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(held)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "approved" {
		t.Fatalf("held contents = %q", contents)
	}
}
