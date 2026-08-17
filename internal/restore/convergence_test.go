package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
)

func TestBuildPlanPlansOnlySupportedExtraCSSAndArtworkRemoval(t *testing.T) {
	created, _ := fixtureSnapshot(t)
	target := targetPaths(t)
	extraTheme := filepath.Join(target.Decky, "themes", "Extra Theme")
	if err := os.MkdirAll(extraTheme, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extraTheme, "theme.json"), []byte(`{"name":"Extra Theme","version":"1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extraTheme, "config_USER.json"), []byte(`{"active":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	grid := filepath.Join(target.Steam, "userdata", "100000009", "config", "grid")
	if err := os.MkdirAll(grid, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grid, "900000099.png"), []byte("supported extra artwork"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), PlanOptions{Paths: target, SnapshotPath: created.Path, AppVersion: "phase3-test", Now: time.Date(2026, 8, 16, 20, 0, 0, 0, time.UTC), Limits: limits.Default(), Resolver: staticResolver{url: "https://example.test/package.zip", hash: strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	removedTheme, removedArtwork := false, false
	for _, action := range plan.Actions {
		if action.Operation != "remove" {
			continue
		}
		if strings.HasPrefix(action.LogicalPath, "css-loader/themes/Extra Theme/") {
			removedTheme = true
		}
		if action.LogicalPath == "steam/artwork/userdata/100000009/grid/900000099.png" {
			removedArtwork = true
		}
	}
	if !removedTheme || !removedArtwork {
		t.Fatalf("supported convergence removals missing: %#v", plan.Actions)
	}
	if err := Revalidate(plan, limits.Default()); err != nil {
		t.Fatalf("convergence plan did not bind removal state: %v", err)
	}
	tampered := plan
	for index := range tampered.Actions {
		if tampered.Actions[index].Operation == "remove" {
			tampered.Actions[index].ExistingSHA256 = strings.Repeat("0", 64)
			break
		}
	}
	if err := ValidatePlan(tampered); err == nil {
		t.Fatal("removal action did not alter the sealed approval")
	}
}

func TestBuildPlanPlansVerifiedExtraPluginRemoval(t *testing.T) {
	created, _ := fixtureSnapshot(t)
	target := targetPaths(t)
	if err := os.MkdirAll(filepath.Join(target.Decky, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target.Decky, "settings", "loader.json"), []byte(`{"pluginOrder":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(target.Decky, "plugins", "extra-plugin")
	if err := replacePluginFixture(extra, pluginTreeFixture{name: "Extra Plugin", author: "Deck Snapshot Tests", version: "1.0.0", payload: "extra"}, 0o644); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDeckyInstaller{target: extra}
	plan, err := BuildPlan(context.Background(), PlanOptions{Paths: target, SnapshotPath: created.Path, AppVersion: "phase3-test", Now: time.Date(2026, 8, 16, 20, 0, 0, 0, time.UTC), Limits: limits.Default(), Resolver: staticResolver{url: "https://example.test/package.zip", hash: strings.Repeat("a", 64)}, DeckyInstaller: fake})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range plan.PluginActions {
		if action.Directory == "extra-plugin" {
			if action.Operation != "remove" || action.Method != pluginMethodDeckyAPI || action.ExistingName != "Extra Plugin" {
				t.Fatalf("extra plugin action = %#v", action)
			}
			if plan.DeckyLoaderGuard == nil {
				t.Fatal("extra plugin removal lacks Decky settings recovery guard")
			}
			return
		}
	}
	t.Fatal("extra plugin removal was not planned")
}

func TestBuildPlanBlocksAmbiguousExtraPluginRemoval(t *testing.T) {
	created, _ := fixtureSnapshot(t)
	target := targetPaths(t)
	if err := os.MkdirAll(filepath.Join(target.Decky, "settings"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target.Decky, "settings", "loader.json"), []byte(`{"pluginOrder":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"extra-one", "extra-two"} {
		if err := replacePluginFixture(filepath.Join(target.Decky, "plugins", directory), pluginTreeFixture{name: "Ambiguous Plugin", author: "Deck Snapshot Tests", version: "1.0.0", payload: directory}, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeDeckyInstaller{target: filepath.Join(target.Decky, "plugins", "extra-one")}
	plan, err := BuildPlan(context.Background(), PlanOptions{Paths: target, SnapshotPath: created.Path, AppVersion: "phase3-test", Now: time.Date(2026, 8, 16, 20, 0, 0, 0, time.UTC), Limits: limits.Default(), Resolver: staticResolver{url: "https://example.test/package.zip", hash: strings.Repeat("a", 64)}, DeckyInstaller: fake})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Blocking {
		t.Fatal("ambiguous extra plugin removal was not blocked")
	}
	for _, action := range plan.PluginActions {
		if action.Directory == "extra-one" || action.Directory == "extra-two" {
			if action.Operation != "blocked" || !strings.Contains(action.Reason, "ambiguous") {
				t.Fatalf("ambiguous action = %#v", action)
			}
		}
	}
}

func TestRemovalRefusesModeChangeAfterApproval(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mode-safe deletion uses the Linux restore target semantics")
	}
	home := t.TempDir()
	target := filepath.Join(home, "homebrew", "themes", "extra", "theme.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte(`{"name":"Extra"}`)
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	action := Action{TargetPath: target, Size: int64(len(contents)), SHA256: hex.EncodeToString(digest[:]), ExistingMode: 0o600}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeAppliedCreate(home, action); err == nil {
		t.Fatal("removal accepted a target whose mode changed after approval")
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatalf("mode-changed target was removed: %v", err)
	}
}

func TestRollbackRemovalUsesCreatedTargetMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mode-safe deletion uses the Linux restore target semantics")
	}
	home := t.TempDir()
	target := filepath.Join(home, "homebrew", "themes", "fixture", "theme.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte(`{"name":"Fixture"}`)
	if err := os.WriteFile(target, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	action := Action{TargetPath: target, Operation: "create", Size: int64(len(contents)), SHA256: hex.EncodeToString(digest[:]), DesiredMode: 0o600}
	if err := removeAppliedCreate(home, action); err != nil {
		t.Fatalf("remove created target: %v", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created target remains after rollback removal: %v", err)
	}
}
