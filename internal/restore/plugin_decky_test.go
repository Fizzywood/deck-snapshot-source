package restore

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

type pluginTreeFixture struct {
	name    string
	author  string
	version string
	payload string
}

type fakeDeckyInstaller struct {
	target          string
	fixtures        map[string]pluginTreeFixture
	failNextInstall bool
	probeCalls      int
	installCalls    []deckyapi.InstallRequest
	uninstallCalls  []string
}

func (fake *fakeDeckyInstaller) Probe(context.Context, string) error {
	fake.probeCalls++
	return nil
}

func (fake *fakeDeckyInstaller) Install(_ context.Context, request deckyapi.InstallRequest) error {
	fake.installCalls = append(fake.installCalls, request)
	fixture, exists := fake.fixtures[request.Version]
	if !exists {
		return errors.New("unexpected fixture version")
	}
	if err := replacePluginFixture(fake.target, fixture, 0o644); err != nil {
		return err
	}
	if fake.failNextInstall {
		fake.failNextInstall = false
		return errors.New("simulated Decky failure after mutation")
	}
	return nil
}

func (fake *fakeDeckyInstaller) Uninstall(_ context.Context, name string) error {
	fake.uninstallCalls = append(fake.uninstallCalls, name)
	for _, filename := range []string{"payload.txt", "package.json", "plugin.json"} {
		if err := os.Remove(filepath.Join(fake.target, filename)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Remove(fake.target)
}

func TestDeckyInstallCreateAndRollback(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "plugins", "fixture-plugin")
	if err := os.Mkdir(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	desired := pluginTreeFixture{name: "Fixture Plugin", author: "Fixture Author", version: "2.0.0", payload: "new payload"}
	prepared, resolution := preparedPluginFixture(t, "fixture-plugin", desired)
	action := PluginAction{Directory: "fixture-plugin", Method: pluginMethodDeckyAPI, TargetPath: target, Operation: "create"}
	fake := &fakeDeckyInstaller{target: target, fixtures: map[string]pluginTreeFixture{desired.version: desired}}

	installed, err := installPreparedPluginWithDecky(context.Background(), fake, action, prepared, resolution, limits.Default())
	if err != nil {
		t.Fatalf("installPreparedPluginWithDecky() error = %v", err)
	}
	if !installed.MutationStarted || installed.NewFingerprint == "" || len(fake.installCalls) != 1 || fake.installCalls[0].Replace {
		t.Fatalf("installed = %#v, calls = %#v", installed, fake.installCalls)
	}
	if err := rollbackInstalledPluginsWithDecky(context.Background(), root, []installedPlugin{installed}, limits.Default(), fake, nil); err != nil {
		t.Fatalf("rollbackInstalledPluginsWithDecky() error = %v", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) || len(fake.uninstallCalls) != 1 || fake.uninstallCalls[0] != desired.name {
		t.Fatalf("target error = %v, uninstall calls = %#v", err, fake.uninstallCalls)
	}
}

func TestDeckyReplaceRollbackRestoresRecoveryIdentity(t *testing.T) {
	for _, failAfterMutation := range []bool{false, true} {
		failAfterMutation := failAfterMutation
		t.Run(map[bool]string{false: "successful install", true: "reported failure after mutation"}[failAfterMutation], func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			target := filepath.Join(root, "plugins", "fixture-plugin")
			old := pluginTreeFixture{name: "Fixture Plugin", author: "Fixture Author", version: "1.0.0", payload: "old payload"}
			desired := pluginTreeFixture{name: "Fixture Plugin", author: "Fixture Author", version: "2.0.0", payload: "new payload"}
			if err := replacePluginFixture(target, old, 0o644); err != nil {
				t.Fatal(err)
			}
			oldFingerprint, oldFiles, oldBytes, err := fingerprintDeckyManagedPluginTree(target, limits.Default())
			if err != nil {
				t.Fatal(err)
			}
			prepared, resolution := preparedPluginFixture(t, "fixture-plugin", desired)
			action := PluginAction{Directory: "fixture-plugin", Method: pluginMethodDeckyAPI, TargetPath: target, Operation: "replace", ExistingFingerprint: oldFingerprint, ExistingVersion: old.version, ExistingFiles: oldFiles, ExistingBytes: oldBytes}
			fake := &fakeDeckyInstaller{target: target, fixtures: map[string]pluginTreeFixture{old.version: old, desired.version: desired}, failNextInstall: failAfterMutation}

			installed, installErr := installPreparedPluginWithDecky(context.Background(), fake, action, prepared, resolution, limits.Default())
			if failAfterMutation && installErr == nil {
				t.Fatal("install error = nil, want simulated error")
			}
			if !failAfterMutation && installErr != nil {
				t.Fatalf("install error = %v", installErr)
			}
			if !installed.MutationStarted {
				t.Fatal("mutation was not tracked")
			}
			recoveryPath, recoveryHash := writeOpaqueArchive(t, "validated recovery")
			recovery := map[string]recoveryPluginPackage{"fixture-plugin": {Archive: recoveryPath, SHA256: recoveryHash}}
			if err := rollbackInstalledPluginsWithDecky(context.Background(), root, []installedPlugin{installed}, limits.Default(), fake, recovery); err != nil {
				t.Fatalf("rollback error = %v", err)
			}
			fingerprint, files, bytes, err := fingerprintDeckyManagedPluginTree(target, limits.Default())
			if err != nil || fingerprint != oldFingerprint || files != oldFiles || bytes != oldBytes {
				t.Fatalf("restored identity = %s/%d/%d, error = %v", fingerprint, files, bytes, err)
			}
			if len(fake.installCalls) != 2 || fake.installCalls[1].Version != old.version || !fake.installCalls[1].Replace {
				t.Fatalf("install calls = %#v", fake.installCalls)
			}
		})
	}
}

func TestDeckyRecoveryPackageUsesValidatedStagedInventory(t *testing.T) {
	t.Parallel()
	directory := "fixture-plugin"
	contents := map[string]string{
		"plugin.json":  `{"name":"Fixture Plugin","author":"Fixture Author"}`,
		"package.json": `{"version":"1.0.0"}`,
		"payload.txt":  "old payload",
	}
	value := manifest.New("recovery-test", "test", "test", "test", time.Unix(1, 0).UTC())
	staged := make(map[string]snapshot.StagedFile)
	var total int64
	for name, content := range contents {
		logicalPath := "recovery/plugins/" + directory + "/" + name
		path, checksum := writeOpaqueArchive(t, content)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		entry := manifest.File{LogicalPath: logicalPath, Component: "recovery/plugins", Size: info.Size(), SHA256: checksum, Mode: 0o644}
		value.Files = append(value.Files, entry)
		staged[logicalPath] = snapshot.StagedFile{LogicalPath: logicalPath, Path: path, Size: info.Size(), SHA256: checksum}
		total += info.Size()
	}
	value.Normalize()
	plan := Plan{PluginActions: []PluginAction{{Directory: directory, Method: pluginMethodDeckyAPI, Operation: "replace", ExistingFiles: len(contents), ExistingBytes: total}}}
	if err := validateStagedRecovery(plan, value, value, staged); err != nil {
		t.Fatalf("validateStagedRecovery() error = %v", err)
	}
	packages, err := createRecoveryPluginPackages(context.Background(), filepath.Join(t.TempDir(), "packages"), plan, value, staged, limits.Default())
	if err != nil {
		t.Fatalf("createRecoveryPluginPackages() error = %v", err)
	}
	created := packages[directory]
	checksum, _, err := hashRegularFile(created.Archive, pluginstore.MaxPackageTotal)
	if err != nil || checksum != created.SHA256 {
		t.Fatalf("archive checksum = %q, error = %v", checksum, err)
	}
	reader, err := zip.OpenReader(created.Archive)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	seen := make(map[string]string)
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		input, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(input)
		input.Close()
		if err != nil {
			t.Fatal(err)
		}
		seen[entry.Name] = string(data)
	}
	for name, content := range contents {
		if seen[directory+"/"+name] != content {
			t.Fatalf("archive entry %q = %q", name, seen[directory+"/"+name])
		}
	}
	delete(staged, "recovery/plugins/"+directory+"/payload.txt")
	if err := validateStagedRecovery(plan, value, value, staged); err == nil {
		t.Fatal("validateStagedRecovery() error = nil after removing a plugin payload")
	}
}

func TestDeckyAPIMutationRequiresRecoverableSettingsSideEffects(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	paths := platform.Paths{Home: root, Decky: filepath.Join(root, "homebrew")}
	settingsRoot := filepath.Join(paths.Decky, "settings")
	if err := os.MkdirAll(settingsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsRoot, "loader.json"), []byte(`{"pluginOrder":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := Plan{
		Actions:       []Action{{LogicalPath: "decky/settings/loader.json", Operation: "unchanged"}},
		PluginActions: []PluginAction{{Directory: "fixture-plugin", Method: pluginMethodDeckyAPI, Operation: "replace"}},
	}
	bindDeckyAPISettingsRecovery(&plan, paths, limits.Default())
	if plan.Actions[0].Operation != "unchanged" || plan.DeckyLoaderGuard == nil || plan.DeckyLoaderGuard.Operation != "restore" || len(plan.Warnings) != 1 || plan.PluginActions[0].Operation != "replace" {
		t.Fatalf("plan = %#v", plan)
	}
	blocked := Plan{PluginActions: []PluginAction{{Directory: "fixture-plugin", Method: pluginMethodDeckyAPI, Operation: "create"}}}
	bindDeckyAPISettingsRecovery(&blocked, platform.Paths{}, limits.Default())
	if blocked.PluginActions[0].Operation != "blocked" || blocked.PluginActions[0].Method != "" || blocked.PluginActions[0].Reason == "" {
		t.Fatalf("blocked plan = %#v", blocked)
	}
}

func TestDeckyLoaderGuardIsCapturedAndStagedFromCurrentTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	settingsRoot := filepath.Join(root, "homebrew", "settings")
	target := filepath.Join(settingsRoot, "loader.json")
	contents := []byte(`{"pluginOrder":["Fixture Plugin"]}`)
	if err := os.MkdirAll(settingsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	guard := &LoaderSettingsGuard{
		TargetRoot:     settingsRoot,
		TargetPath:     target,
		Operation:      "restore",
		ExistingSize:   int64(len(contents)),
		ExistingSHA256: hex.EncodeToString(digest[:]),
		ExistingMode:   uint32(targetInfo.Mode().Perm()),
	}
	plan := Plan{PlanID: "restore-test", AppVersion: "test", Target: TargetReference{Home: root}, DeckyLoaderGuard: guard}
	created, err := createRecovery(context.Background(), plan, filepath.Join(root, "recovery-output"), time.Unix(1, 0).UTC(), limits.Default())
	if err != nil {
		t.Fatalf("createRecovery() error = %v", err)
	}
	if len(created.Manifest.Files) != 1 || created.Manifest.Files[0].LogicalPath != deckyLoaderRecoveryLogicalPath {
		t.Fatalf("recovery files = %#v", created.Manifest.Files)
	}
	stageRoot := filepath.Join(root, "staged")
	if err := os.Mkdir(stageRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	staged, stagedManifest, err := snapshot.StageSelected(context.Background(), created.Path, stageRoot, []string{deckyLoaderRecoveryLogicalPath}, limits.Default())
	if err != nil {
		t.Fatalf("StageSelected() error = %v", err)
	}
	if err := validateStagedRecovery(plan, created.Manifest, stagedManifest, staged); err != nil {
		t.Fatalf("validateStagedRecovery() error = %v", err)
	}
	delete(staged, deckyLoaderRecoveryLogicalPath)
	if err := validateStagedRecovery(plan, created.Manifest, stagedManifest, staged); err == nil {
		t.Fatal("validateStagedRecovery() accepted a missing loader settings guard payload")
	}
}

func preparedPluginFixture(t *testing.T, directory string, fixture pluginTreeFixture) (pluginstore.PreparedPackage, pluginstore.Resolution) {
	t.Helper()
	workspace := t.TempDir()
	root := filepath.Join(workspace, directory)
	if err := replacePluginFixture(root, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	archive, checksum := writeOpaqueArchive(t, "verified official archive")
	metadata := pluginstore.PackageMetadata{Name: fixture.name, Author: fixture.author, Version: fixture.version}
	prepared := pluginstore.PreparedPackage{Directory: directory, Root: root, Archive: archive, Metadata: metadata, Files: 3}
	resolution := pluginstore.Resolution{SnapshotDirectory: directory, Status: "resolved", StoreName: fixture.name, StoreAuthor: fixture.author, ResolvedVersion: fixture.version, SHA256: checksum}
	return prepared, resolution
}

func replacePluginFixture(root string, fixture pluginTreeFixture, mode os.FileMode) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"plugin.json":  `{"name":"` + fixture.name + `","author":"` + fixture.author + `"}`,
		"package.json": `{"version":"` + fixture.version + `"}`,
		"payload.txt":  fixture.payload,
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), mode); err != nil {
			return err
		}
	}
	return nil
}

func writeOpaqueArchive(t *testing.T, contents string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "package.zip")
	data := []byte(contents)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	return path, hex.EncodeToString(hash[:])
}
