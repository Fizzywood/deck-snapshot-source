package cloud

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPasswordCreateLoadAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "cloud", "config-password")
	first, err := LoadOrCreateConfigPassword(path)
	if err != nil || !validConfigPassword(first) {
		t.Fatalf("LoadOrCreateConfigPassword() = %q, %v", first, err)
	}
	second, err := LoadOrCreateConfigPassword(path)
	if err != nil || second != first {
		t.Fatalf("second LoadOrCreateConfigPassword() = %q, %v", second, err)
	}
	loaded, err := LoadConfigPassword(path)
	if err != nil || loaded != first {
		t.Fatalf("LoadConfigPassword() = %q, %v", loaded, err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("configuration key is unsafe: %#v, %v", info, err)
	}
	if err := RemoveConfigPassword(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration key remained after removal: %v", err)
	}
}

func TestSaveLegacyConfigPasswordNoReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "cloud", "config-password")
	if err := SaveConfigPassword(path, "legacy-configuration-password"); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadConfigPassword(path); err != nil || got != "legacy-configuration-password" {
		t.Fatalf("LoadConfigPassword() = %q, %v", got, err)
	}
	if err := SaveConfigPassword(path, "different-configuration-password"); err == nil {
		t.Fatal("SaveConfigPassword() replaced an existing key")
	}
}
