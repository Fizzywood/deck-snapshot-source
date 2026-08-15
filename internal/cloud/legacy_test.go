package cloud

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPreserveLegacyConnectionIsPrivateAndIdempotent(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "active")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(source, "rclone.conf")
	passwordPath := filepath.Join(source, "config-password")
	if err := os.WriteFile(configPath, []byte("synthetic encrypted legacy config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwordPath, []byte("legacy-configuration-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "cloud", LegacyConnectionDirectoryName)
	if err := PreserveLegacyConnection(configPath, passwordPath, destination); err != nil {
		t.Fatal(err)
	}
	if err := PreserveLegacyConnection(configPath, passwordPath, destination); err != nil {
		t.Fatalf("idempotent PreserveLegacyConnection() error = %v", err)
	}
	for _, name := range []string{"rclone.conf", "config-password"} {
		info, err := os.Lstat(filepath.Join(destination, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("preserved %s is unsafe: %#v, %v", name, info, err)
		}
	}
	if err := os.WriteFile(configPath, []byte("different legacy connection\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PreserveLegacyConnection(configPath, passwordPath, destination); err == nil {
		t.Fatal("PreserveLegacyConnection() replaced a different preserved connection")
	}
}

func TestPreserveLegacyConnectionWithPasswordDoesNotNeedActivePasswordFile(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "active")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(source, "rclone.conf")
	if err := os.WriteFile(configPath, []byte("synthetic encrypted legacy config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "cloud", LegacyConnectionDirectoryName)
	if err := PreserveLegacyConnectionWithPassword(configPath, "legacy-configuration-password", destination); err != nil {
		t.Fatal(err)
	}
	password, err := LoadConfigPassword(filepath.Join(destination, "config-password"))
	if err != nil || password != "legacy-configuration-password" {
		t.Fatalf("preserved password was not private and readable: %q, %v", password, err)
	}
	if _, err := os.Lstat(filepath.Join(source, "config-password")); !os.IsNotExist(err) {
		t.Fatalf("active password file was created unexpectedly: %v", err)
	}
}
