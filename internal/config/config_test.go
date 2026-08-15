package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingReturnsDefaults(t *testing.T) {
	defaults := Config{SchemaVersion: CurrentSchemaVersion, LogLevel: "info", SnapshotDirectory: filepath.Join(t.TempDir(), "snapshots")}
	got, err := Load(filepath.Join(t.TempDir(), "missing.json"), defaults)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got != defaults {
		t.Fatalf("Load() = %#v, want %#v", got, defaults)
	}
}

func TestLoadStrictConfiguration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	contents := `{"schema_version":1,"log_level":"debug","snapshot_directory":` + quote(filepath.Join(root, "snapshots")) + `}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := Config{SchemaVersion: CurrentSchemaVersion, LogLevel: "info", SnapshotDirectory: filepath.Join(root, "default-snapshots")}
	got, err := Load(path, defaults)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.LogLevel != "debug" || got.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("Load() did not migrate schema 1 with safe defaults: %#v", got)
	}
}

func TestSaveAndLoadSettings(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "nested", "deck-snapshot", "config.json")
	want := Config{
		SchemaVersion: CurrentSchemaVersion, LogLevel: "warn", SnapshotDirectory: filepath.Join(root, "snapshots"),
		AutoUpload: true, RecoveryFile: filepath.Join(root, "separate", "recovery.json"),
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load(path, Config{})
	if err != nil || got != want {
		t.Fatalf("Load(Save()) = %#v, %v; want %#v", got, err, want)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("saved configuration is unsafe: %#v, %v", info, err)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	contents := `{"schema_version":1,"log_level":"info","snapshot_directory":` + quote(root) + `,"token":"must-not-be-accepted"}`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, Config{})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load() error = %v, want unknown-field error", err)
	}
}

func quote(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}
