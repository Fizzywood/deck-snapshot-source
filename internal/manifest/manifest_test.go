package manifest

import (
	"strings"
	"testing"
	"time"
)

func TestManifestValidate(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	value := New("snapshot-1", "test", "device-1", "", now)
	value.Files = []File{{LogicalPath: "decky/settings/plugin/config.json", Component: "decky", Size: 2, SHA256: strings.Repeat("a", 64), Mode: 0o600}}
	if err := value.Validate(1024); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestManifestRejectsUnknownMajorVersion(t *testing.T) {
	value := New("snapshot-1", "test", "device-1", "", time.Now())
	value.FormatVersion = "2.0"
	if err := value.Validate(1024); err == nil {
		t.Fatal("Validate() accepted an unknown major version")
	}
}

func TestValidateLogicalPathRejectsUnsafePaths(t *testing.T) {
	unsafe := []string{"", ".", "../escape", "safe/../../escape", "/absolute", `C:\\absolute`, `safe\\windows`, "safe//double", "safe/./dot", "safe\x00nul", "theme/e\u0301.css", "safe/file:stream", "safe/NUL.txt", "safe/COM1", "safe/trailing.", "safe/trailing "}
	for _, value := range unsafe {
		if err := ValidateLogicalPath(value, 1024); err == nil {
			t.Errorf("ValidateLogicalPath(%q) accepted an unsafe path", value)
		}
	}
}

func TestManifestRejectsDuplicateMetadataCollections(t *testing.T) {
	value := New("snapshot-1", "test", "device-1", "", time.Now())
	value.Accounts = []SteamAccount{{ID: "123"}, {ID: "123"}}
	if err := value.Validate(1024); err == nil {
		t.Fatal("Validate() accepted duplicate account metadata")
	}
	value = New("snapshot-1", "test", "device-1", "", time.Now())
	value.Plugins = []Plugin{{Directory: "Example", Name: "One"}, {Directory: "example", Name: "Two"}}
	if err := value.Validate(1024); err == nil {
		t.Fatal("Validate() accepted case-colliding plugin metadata")
	}
}

func TestManifestRejectsDuplicatePaths(t *testing.T) {
	value := New("snapshot-1", "test", "device-1", "", time.Now())
	entry := File{LogicalPath: "decky/data/a.json", Component: "decky", Size: 1, SHA256: strings.Repeat("b", 64)}
	value.Files = []File{entry, entry}
	if err := value.Validate(1024); err == nil {
		t.Fatal("Validate() accepted duplicate file paths")
	}
}

func TestManifestRejectsUnsafeDetectedVersion(t *testing.T) {
	value := New("snapshot-1", "test", "device-1", "", time.Now())
	value.Detected.DeckyVersion = "v3.2.6\nforged"
	if err := value.Validate(1024); err == nil {
		t.Fatal("Validate() accepted unsafe detected-version metadata")
	}
}

func TestManifestRejectsUnsafeSnapshotIDAndExclusion(t *testing.T) {
	value := New("../unsafe", "test", "device-1", "", time.Now())
	if err := value.Validate(1024); err == nil {
		t.Fatal("Validate() accepted an unsafe snapshot ID")
	}
	value = New("safe-id", "test", "device-1", "", time.Now())
	value.Exclusions = []Exclusion{{LogicalPath: "../unsafe", Component: "decky", Reason: "test"}}
	if err := value.Validate(1024); err == nil {
		t.Fatal("Validate() accepted an unsafe exclusion path")
	}
}
