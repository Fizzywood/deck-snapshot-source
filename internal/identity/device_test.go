package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateIsStable(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	first, err := LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreate(state)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !deviceIDPattern.MatchString(first) {
		t.Fatalf("device IDs differ or are invalid: %q %q", first, second)
	}
}

func TestLoadOrCreateRejectsInvalidStoredID(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "device-id"), []byte("not-valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(state); err == nil {
		t.Fatal("LoadOrCreate() accepted an invalid stored ID")
	}
}
