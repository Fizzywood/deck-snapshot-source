//go:build linux

package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/discovery"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

func TestBuildPluginActionsUsesDeckyBoundaryForNonWritableRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the non-writable directory access check")
	}
	root := t.TempDir()
	paths := platform.Paths{Home: root, Decky: filepath.Join(root, "homebrew"), State: filepath.Join(root, "state")}
	pluginRoot := filepath.Join(paths.Decky, "plugins")
	pluginPath := filepath.Join(pluginRoot, "fixture-plugin")
	if err := replacePluginFixture(pluginPath, pluginTreeFixture{name: "Fixture Plugin", author: "Fixture Author", version: "1.0.0", payload: "old"}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(paths.State, "preserved"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pluginRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pluginRoot, 0o700) })
	installer := &fakeDeckyInstaller{}
	resolutions := []pluginstore.Resolution{{SnapshotDirectory: "fixture-plugin", Status: "resolved", StoreName: "Fixture Plugin", StoreAuthor: "Fixture Author", ResolvedVersion: "2.0.0"}}
	actions, err := buildPluginActions(context.Background(), paths, "snapshot-test", resolutions, limits.Default(), installer)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Operation != "replace" || actions[0].Method != pluginMethodDeckyAPI || actions[0].ExistingVersion != "1.0.0" || installer.probeCalls != 1 {
		t.Fatalf("actions = %#v, probe calls = %d", actions, installer.probeCalls)
	}
}

func TestRestoreDeckySettingsSideEffectsReinstatesApprovedBytes(t *testing.T) {
	root := t.TempDir()
	settingsRoot := filepath.Join(root, "homebrew", "settings")
	target := filepath.Join(settingsRoot, "loader.json")
	approved := []byte(`{"pluginOrder":["Fixture Plugin"]}`)
	mutated := []byte(`{"pluginOrder":[]}`)
	if err := os.MkdirAll(settingsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	stagedPath := filepath.Join(root, "approved.bin")
	if err := os.WriteFile(stagedPath, approved, 0o600); err != nil {
		t.Fatal(err)
	}
	approvedHash := sha256.Sum256(approved)
	guard := &LoaderSettingsGuard{TargetRoot: settingsRoot, TargetPath: target, Operation: "restore", ExistingSize: int64(len(approved)), ExistingSHA256: hex.EncodeToString(approvedHash[:]), ExistingMode: 0o600}
	recovery := map[string]snapshot.StagedFile{deckyLoaderRecoveryLogicalPath: {LogicalPath: deckyLoaderRecoveryLogicalPath, Path: stagedPath, Size: int64(len(approved)), SHA256: guard.ExistingSHA256}}
	if err := restoreDeckySettingsSideEffects(context.Background(), root, guard, recovery, limits.Default()); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(approved) {
		t.Fatalf("restored contents = %q", contents)
	}
}

func TestRestoreDeckySettingsSideEffectsRemovesNewFile(t *testing.T) {
	root := t.TempDir()
	settingsRoot := filepath.Join(root, "homebrew", "settings")
	target := filepath.Join(settingsRoot, "loader.json")
	contents := []byte(`{"pluginOrder":[]}`)
	if err := os.MkdirAll(settingsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	guard := &LoaderSettingsGuard{TargetRoot: settingsRoot, TargetPath: target, Operation: "remove"}
	if err := restoreDeckySettingsSideEffects(context.Background(), root, guard, nil, limits.Default()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loader settings target still exists: %v", err)
	}
}

func TestDeckyAPIPlanRevalidatesWithBoundLoaderSettings(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the non-writable directory access check")
	}
	sourceHome := filepath.Join(t.TempDir(), "source")
	source := platform.Paths{Home: sourceHome, Decky: filepath.Join(sourceHome, "homebrew"), Steam: filepath.Join(sourceHome, ".local", "share", "Steam")}
	if err := replacePluginFixture(filepath.Join(source.Decky, "plugins", "fixture-plugin"), pluginTreeFixture{name: "Fixture Plugin", author: "Fixture Author", version: "2.0.0", payload: "desired"}, 0o600); err != nil {
		t.Fatal(err)
	}
	loaderContents := []byte(`{"pluginOrder":["Fixture Plugin"]}`)
	if err := os.MkdirAll(source.Steam, 0o700); err != nil {
		t.Fatal(err)
	}
	discovered, err := discovery.Discover(context.Background(), discovery.Options{Paths: source, AppVersion: "test", DeviceID: "ds-00000000000000000000000000000001", SnapshotID: "dsnap-decky-api", Now: time.Unix(1, 0).UTC(), Limits: limits.Default()})
	if err != nil {
		t.Fatal(err)
	}
	created, err := snapshot.Create(context.Background(), filepath.Join(t.TempDir(), "snapshots"), discovered, limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	targetHome := filepath.Join(t.TempDir(), "target")
	target := platform.Paths{Home: targetHome, Decky: filepath.Join(targetHome, "homebrew"), Steam: filepath.Join(targetHome, ".local", "share", "Steam"), State: filepath.Join(targetHome, ".local", "state", "deck-snapshot")}
	pluginRoot := filepath.Join(target.Decky, "plugins")
	if err := replacePluginFixture(filepath.Join(pluginRoot, "fixture-plugin"), pluginTreeFixture{name: "Fixture Plugin", author: "Fixture Author", version: "1.0.0", payload: "old"}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target.Decky, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target.Decky, "settings", "loader.json"), loaderContents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target.Steam, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target.State, "preserved"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pluginRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pluginRoot, 0o700) })
	installer := &fakeDeckyInstaller{}
	resolver := staticResolver{url: "https://example.test/plugin.zip", hash: strings.Repeat("a", 64), version: "2.0.0"}
	plan, err := BuildPlan(context.Background(), PlanOptions{Paths: target, SnapshotPath: created.Path, AppVersion: "test", Now: time.Unix(2, 0).UTC(), Limits: limits.Default(), Resolver: resolver, DeckyInstaller: installer})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Blocking || plan.PluginActions[0].Method != pluginMethodDeckyAPI {
		t.Fatalf("plan = %#v", plan)
	}
	if plan.DeckyLoaderGuard == nil || plan.DeckyLoaderGuard.Operation != "restore" {
		t.Fatalf("loader guard = %#v", plan.DeckyLoaderGuard)
	}
	if err := Revalidate(plan, limits.Default(), installer); err != nil {
		t.Fatalf("Revalidate() error = %v", err)
	}
}

func TestBuildPluginActionsKeepsCurrentPluginUnchangedWithoutDeckyProbe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the non-writable directory access check")
	}
	root := t.TempDir()
	paths := platform.Paths{Home: root, Decky: filepath.Join(root, "homebrew"), State: filepath.Join(root, "state")}
	pluginRoot := filepath.Join(paths.Decky, "plugins")
	pluginPath := filepath.Join(pluginRoot, "fixture-plugin")
	if err := replacePluginFixture(pluginPath, pluginTreeFixture{name: "Fixture Plugin", author: "Fixture Author", version: "1.0.0", payload: "current"}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(pluginRoot, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pluginRoot, 0o700) })
	installer := &fakeDeckyInstaller{}
	resolutions := []pluginstore.Resolution{{SnapshotDirectory: "fixture-plugin", Status: "resolved", StoreName: "Fixture Plugin", StoreAuthor: "Fixture Author", ResolvedVersion: "1.0.0"}}
	actions, err := buildPluginActions(context.Background(), paths, "snapshot-test", resolutions, limits.Default(), installer)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Operation != "unchanged" || actions[0].Method != pluginMethodNone || installer.probeCalls != 0 {
		t.Fatalf("actions = %#v, probe calls = %d", actions, installer.probeCalls)
	}
}
