package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cloudcore "github.com/Fizzywood/deck-snapshot/internal/cloud"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

type fakeEnvironment struct{ home string }

type fakeResolver struct{}

func (fakeResolver) Resolve(_ context.Context, plugins []manifest.Plugin) ([]pluginstore.Resolution, error) {
	result := make([]pluginstore.Resolution, 0, len(plugins))
	for index, plugin := range plugins {
		result = append(result, pluginstore.Resolution{SnapshotDirectory: plugin.Directory, SnapshotName: plugin.Name, SnapshotAuthor: plugin.Author, SnapshotVersion: plugin.Version, Status: "resolved", Message: "synthetic", StoreID: int64(index + 1), StoreName: plugin.Name, StoreAuthor: plugin.Author, ResolvedVersion: plugin.Version, SHA256: strings.Repeat("a", 64), ArtifactURL: "https://example.test/package.zip"})
	}
	return result, nil
}

func (f fakeEnvironment) LookupEnv(string) (string, bool) { return "", false }
func (f fakeEnvironment) UserHomeDir() (string, error)    { return f.home, nil }

func TestVersionIsEnglish(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"version"}, &stdout, &stderr, Dependencies{Version: "v0.0.0-test"})
	if code != ExitOK || stdout.String() != "Deck Snapshot v0.0.0-test\n" || stderr.Len() != 0 {
		t.Fatalf("Run(version) = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestSnapshotDetailsEmitNoticesOnlyOnRequest(t *testing.T) {
	response := snapshotDetailsResponse{CreatedUTC: "2026-08-16T12:00:00Z", Size: 42, Files: 1, FileBytes: 42, Warnings: []manifest.Warning{{Code: "unsupported_grid_file", Component: "steam", Message: "A non-image grid file was excluded."}}}
	var stdout, stderr bytes.Buffer
	if code := writeSnapshotDetails(&stdout, &stderr, false, false, response); code != ExitOK || strings.Contains(stdout.String(), "Notice:") {
		t.Fatalf("concise details = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := writeSnapshotDetails(&stdout, &stderr, false, true, response); code != ExitOK || !strings.Contains(stdout.String(), "Notice: unsupported_grid_file\tsteam\tA non-image grid file was excluded.\n") {
		t.Fatalf("detailed details = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestPathsJSON(t *testing.T) {
	root := filepath.Join(t.TempDir(), "person")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"paths", "--json"}, &stdout, &stderr, Dependencies{Environment: fakeEnvironment{home: root}})
	if code != ExitOK {
		t.Fatalf("Run(paths) = code %d, stderr %q", code, stderr.String())
	}
	var paths platform.Paths
	if err := json.Unmarshal(stdout.Bytes(), &paths); err != nil {
		t.Fatalf("paths output is not JSON: %v", err)
	}
	if paths.Home != root || paths.Decky != filepath.Join(root, "homebrew") {
		t.Fatalf("unexpected paths: %#v", paths)
	}
}

func TestSettingsShowAndSetJSON(t *testing.T) {
	root := filepath.Join(t.TempDir(), "person")
	dependencies := Dependencies{Environment: fakeEnvironment{home: root}}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"settings", "show", "--json"}, &stdout, &stderr, dependencies)
	if code != ExitOK || stderr.Len() != 0 {
		t.Fatalf("settings show = code %d, stderr %q", code, stderr.String())
	}
	var shown settingsResponse
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil || !shown.Settings.AutoUpload {
		t.Fatalf("default settings = %#v, %v", shown, err)
	}
	recovery := filepath.Join(root, "separate", "recovery.json")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"settings", "set", "--auto-upload", "false", "--recovery-file", recovery, "--json"}, &stdout, &stderr, dependencies)
	if code != ExitOK || stderr.Len() != 0 {
		t.Fatalf("settings set = code %d, stderr %q", code, stderr.String())
	}
	var saved settingsResponse
	if err := json.Unmarshal(stdout.Bytes(), &saved); err != nil || saved.Settings.AutoUpload || saved.Settings.RecoveryFile != recovery {
		t.Fatalf("saved settings = %#v, %v", saved, err)
	}
	if info, err := os.Lstat(saved.Path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("settings file is missing or unsafe: %#v, %v", info, err)
	}
}

func TestDoctorIsReadOnlyAndEnglish(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "tests", "fixtures", "deck-home"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"doctor", "--home", root}, &stdout, &stderr, Dependencies{Version: "test", Now: func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }})
	if code != ExitOK || !strings.Contains(stdout.String(), "Deck Snapshot diagnostics:") || !strings.Contains(stdout.String(), "Plugins: 1") || stderr.Len() != 0 {
		t.Fatalf("Run(doctor) = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestCloudCommandRequiresRecoveryMaterial(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"cloud", "upload"}, &stdout, &stderr, Dependencies{})
	if code != ExitUsage || !strings.Contains(stderr.String(), "requires exactly one") {
		t.Fatalf("Run(cloud upload) = code %d, stderr %q", code, stderr.String())
	}
}

func TestCloudLegacyModeIsReadOnly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"cloud", "upload", "--legacy", "snapshot.tar.gz"}, &stdout, &stderr, Dependencies{})
	if code != ExitUsage || !strings.Contains(stderr.String(), "not allowed") {
		t.Fatalf("legacy cloud upload = code %d, stderr %q", code, stderr.String())
	}
}

func TestCloudRecoveryCreateIsEnglishAndNoReplace(t *testing.T) {
	home := t.TempDir()
	paths, err := platform.Resolve(fakeEnvironment{home: home})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.CloudConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	material, err := cloudcore.GenerateRecovery(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := cloudcore.SaveRecovery(filepath.Join(filepath.Dir(paths.CloudConfig), cloudcore.ManagedRecoveryFileName), material); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "recovery.json")
	dependencies := Dependencies{Environment: fakeEnvironment{home: home}, Now: func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"cloud", "recovery", "create", "--output", path}, &stdout, &stderr, dependencies)
	if code != ExitOK || !strings.Contains(stdout.String(), "Recovery key exported") || stderr.Len() != 0 {
		t.Fatalf("cloud recovery create = code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"cloud", "recovery", "create", "--output", path}, &stdout, &stderr, dependencies)
	if code != ExitRuntime || !strings.Contains(stderr.String(), "not replaced") {
		t.Fatalf("second cloud recovery create = code %d, stderr %q", code, stderr.String())
	}
}

func TestCloudRecoveryCreateDoesNotGenerateUnrelatedKey(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(t.TempDir(), "recovery.json")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"cloud", "recovery", "create", "--output", path}, &stdout, &stderr, Dependencies{Environment: fakeEnvironment{home: home}})
	if code != ExitRuntime || !strings.Contains(stderr.String(), "No managed recovery key") {
		t.Fatalf("unconfigured recovery export = code %d, stderr %q", code, stderr.String())
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unconfigured recovery export created a key: %v", err)
	}
}

func TestCloudConfigurationPasswordRequiresOneBoundedStdinLine(t *testing.T) {
	password, err := readCloudConfigurationPassword(strings.NewReader("configuration-password\n"))
	if err != nil || password != "configuration-password" {
		t.Fatalf("readCloudConfigurationPassword() = %q, %v", password, err)
	}
	if _, err := readCloudConfigurationPassword(strings.NewReader("short\n")); err == nil {
		t.Fatal("readCloudConfigurationPassword() accepted a short password")
	}
}

func TestSnapshotCreateListAndValidateFixture(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "tests", "fixtures", "deck-home"))
	if err != nil {
		t.Fatal(err)
	}
	outputDirectory := t.TempDir()
	dependencies := Dependencies{
		Version: "phase2-test",
		Now: func() time.Time {
			return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
		},
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"snapshot", "create", "--home", root, "--output-dir", outputDirectory, "--device-id", "ds-00000000000000000000000000000000", "--json"}, &stdout, &stderr, dependencies)
	if code != ExitOK {
		t.Fatalf("snapshot create code=%d stderr=%q", code, stderr.String())
	}
	var created createResponse
	if err := json.Unmarshal(stdout.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.Valid || created.Files == 0 {
		t.Fatalf("unexpected creation response: %#v", created)
	}
	if _, err := os.Stat(created.Path); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"snapshot", "inspect", "--json", created.Path}, &stdout, &stderr, dependencies)
	if code != ExitOK {
		t.Fatalf("snapshot inspect code=%d stderr=%q", code, stderr.String())
	}
	var details snapshotDetailsResponse
	if err := json.Unmarshal(stdout.Bytes(), &details); err != nil || !details.Valid || details.SnapshotID != created.SnapshotID || details.Files != created.Files || details.FileBytes <= 0 || len(details.Components) == 0 {
		t.Fatalf("snapshot inspect = %#v, %v", details, err)
	}
	targetHome := filepath.Join(t.TempDir(), "target-home")
	if err := os.Mkdir(targetHome, 0o700); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	dependencies.Environment = fakeEnvironment{home: targetHome}
	dependencies.Resolver = fakeResolver{}
	code = Run([]string{"restore", "plan", "--json", created.Path}, &stdout, &stderr, dependencies)
	if code != ExitOK {
		t.Fatalf("restore plan code=%d stderr=%q", code, stderr.String())
	}
	var planned restorePlanResponse
	if err := json.Unmarshal(stdout.Bytes(), &planned); err != nil || planned.Plan.PlanID == "" || planned.Path == "" {
		t.Fatalf("restore plan response=%#v error=%v", planned, err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"restore", "inspect", "--details", planned.Path}, &stdout, &stderr, dependencies)
	if code != ExitOK || !strings.Contains(stdout.String(), "Plan ID: "+planned.Plan.PlanID) || !strings.Contains(stdout.String(), "File action:") {
		t.Fatalf("restore inspect code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"restore", "plan", "--plan-dir", filepath.Join(targetHome, ".local", "state", "deck-snapshot", "plans-alt"), created.Path}, &stdout, &stderr, dependencies)
	if code != ExitOK || strings.Contains(stdout.String(), "File action:") || !strings.Contains(stdout.String(), "Actions:") {
		t.Fatalf("concise restore plan code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(targetHome, "homebrew", "settings")); !os.IsNotExist(err) {
		t.Fatalf("restore plan mutated a production target: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"snapshot", "list", "--output-dir", outputDirectory, "--json"}, &stdout, &stderr, dependencies)
	if code != ExitOK {
		t.Fatalf("snapshot list code=%d stderr=%q", code, stderr.String())
	}
	var listed []snapshot.Summary
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].Valid {
		t.Fatalf("unexpected list response: %#v", listed)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"snapshot", "validate", "--json", created.Path}, &stdout, &stderr, dependencies)
	if code != ExitOK || !strings.Contains(stdout.String(), `"valid": true`) {
		t.Fatalf("snapshot validate code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"destroy"}, &stdout, &stderr, Dependencies{})
	if code != ExitUsage || !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("Run(destroy) = code %d, stderr %q", code, stderr.String())
	}
}

func TestSnapshotPathOverridesDoNotCollide(t *testing.T) {
	root := t.TempDir()
	var stderr bytes.Buffer
	paths, code := resolveSnapshotPaths(fakeEnvironment{home: root}, "", root, root, root, &stderr)
	if code != ExitOK {
		t.Fatalf("resolveSnapshotPaths() code=%d stderr=%q", code, stderr.String())
	}
	if paths.Decky != root || paths.Steam != root || paths.Snapshots != root {
		t.Fatalf("path override collision: %#v", paths)
	}
}
