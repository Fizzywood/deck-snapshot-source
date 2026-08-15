package discovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
)

func TestDiscoverFixtureIsCompleteAndDeterministic(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "tests", "fixtures", "deck-home"))
	if err != nil {
		t.Fatal(err)
	}
	options := fixtureOptions(root)
	first, err := Discover(context.Background(), options)
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	second, err := Discover(context.Background(), options)
	if err != nil {
		t.Fatalf("second Discover() error = %v", err)
	}
	if len(first.Manifest.Plugins) != 1 || len(first.Manifest.CSSThemes) != 2 || len(first.Manifest.Accounts) != 2 || len(first.Manifest.Artwork) != 6 {
		t.Fatalf("unexpected discovery counts: plugins=%d themes=%d accounts=%d artwork=%d", len(first.Manifest.Plugins), len(first.Manifest.CSSThemes), len(first.Manifest.Accounts), len(first.Manifest.Artwork))
	}
	if !hasFile(first, "decky/settings/loader.json") || !hasFile(first, "decky/settings/fixture-plugin/settings.json") || !hasFile(first, "decky/data/fixture-plugin/state.json") || !hasFile(first, "css-loader/themes/Fixture Theme/theme.json") || !hasFile(first, "steam/artwork/userdata/100000002/grid/900000003_hero.png") {
		t.Fatalf("discovery omitted an expected file: %#v", first.Manifest.Files)
	}
	for _, file := range first.Manifest.Files {
		if strings.HasPrefix(file.LogicalPath, "decky/plugins/") {
			t.Fatalf("plugin binaries entered the snapshot inventory: %s", file.LogicalPath)
		}
	}
	firstJSON, _ := json.Marshal(first.Manifest)
	secondJSON, _ := json.Marshal(second.Manifest)
	if string(firstJSON) != string(secondJSON) {
		t.Fatal("fixture discovery is not deterministic")
	}
}

func TestDiscoverExcludesSecretContentAndSymlink(t *testing.T) {
	root := t.TempDir()
	decky := filepath.Join(root, "homebrew")
	plugin := filepath.Join(decky, "plugins", "fixture")
	settings := filepath.Join(decky, "settings", "fixture")
	if err := os.MkdirAll(plugin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(settings, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(plugin, "plugin.json"), `{"name":"Fixture","author":"Tests"}`)
	mustWrite(t, filepath.Join(plugin, "package.json"), `{"version":"1.0.0"}`)
	mustWrite(t, filepath.Join(settings, "safe.json"), `{"enabled":true}`)
	mustWrite(t, filepath.Join(settings, "auth.json"), `{"access_token":"synthetic-test-value"}`)
	if err := os.MkdirAll(filepath.Join(decky, "themes"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(decky, "themes", "STORE"), "access_token:synthetic-test-value")
	symlinkCreated := os.Symlink(filepath.Join(settings, "safe.json"), filepath.Join(settings, "linked.json")) == nil

	options := fixtureOptions(root)
	options.Paths.Decky = decky
	options.Paths.Steam = filepath.Join(root, "missing-steam")
	result, err := Discover(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFile(result, "decky/settings/fixture/safe.json") || hasFile(result, "decky/settings/fixture/auth.json") {
		t.Fatalf("secret classification failed: %#v", result.Manifest.Files)
	}
	if !hasExclusion(result, "decky/settings/fixture/auth.json", "suspected_secret_content") {
		t.Fatal("secret-content exclusion was not recorded")
	}
	if !hasExclusion(result, "css-loader/themes/STORE", "suspected_secret_content") {
		t.Fatal("CSS Loader STORE secret-content exclusion was not recorded")
	}
	if symlinkCreated && !hasExclusion(result, "decky/settings/fixture/linked.json", "symlink_not_followed") {
		t.Fatal("symlink exclusion was not recorded")
	}
}

func TestDiscoverRecordsOversizedExclusion(t *testing.T) {
	root := t.TempDir()
	decky := filepath.Join(root, "homebrew")
	plugin := filepath.Join(decky, "plugins", "fixture")
	settings := filepath.Join(decky, "settings", "fixture")
	if err := os.MkdirAll(plugin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(settings, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(plugin, "plugin.json"), `{"name":"Fixture"}`)
	mustWrite(t, filepath.Join(plugin, "package.json"), `{"version":"1.0.0"}`)
	if err := os.WriteFile(filepath.Join(settings, "large.bin"), []byte(strings.Repeat("x", 1200)), 0o600); err != nil {
		t.Fatal(err)
	}
	options := fixtureOptions(root)
	options.Paths.Decky = decky
	options.Paths.Steam = filepath.Join(root, "missing-steam")
	options.Limits.MaxFileSize = 1024
	options.Limits.MaxManifestSize = 1024
	options.Limits.MaxTotalSize = 4096
	result, err := Discover(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !hasExclusion(result, "decky/settings/fixture/large.bin", "file_size_limit") {
		t.Fatal("oversized exclusion was not recorded")
	}
}

func TestDiscoverRecordsDetectedSteamOSAndDeckyVersions(t *testing.T) {
	root := t.TempDir()
	decky := filepath.Join(root, "homebrew")
	if err := os.MkdirAll(filepath.Join(decky, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(decky, "services"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(decky, "services", ".loader.version"), "v3.2.6\n")
	osRelease := filepath.Join(root, "os-release")
	mustWrite(t, osRelease, "ID=steamos\nVERSION_ID=\"3.8.16\"\nBUILD_ID=20260716.1\n")
	options := fixtureOptions(root)
	options.Paths.Decky = decky
	options.Paths.Steam = filepath.Join(root, "missing-steam")
	options.OSReleasePath = osRelease
	result, err := Discover(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Manifest.Detected.SteamOSVersion != "3.8.16" || result.Manifest.Detected.DeckyVersion != "v3.2.6" {
		t.Fatalf("detected versions = %#v", result.Manifest.Detected)
	}
}

func fixtureOptions(root string) Options {
	return Options{
		Paths: platform.Paths{
			Home:  root,
			Decky: filepath.Join(root, "homebrew"),
			Steam: filepath.Join(root, ".local", "share", "Steam"),
		},
		AppVersion: "phase2-test",
		DeviceID:   "ds-00000000000000000000000000000000",
		SnapshotID: "dsnap-0000000000000000",
		Now:        time.Date(2026, 8, 14, 12, 0, 0, 123456789, time.FixedZone("fixture", 2*60*60)),
		Limits:     limits.Default(),
	}
}

func hasFile(result Result, logicalPath string) bool {
	for _, file := range result.Manifest.Files {
		if file.LogicalPath == logicalPath {
			return true
		}
	}
	return false
}

func hasExclusion(result Result, logicalPath, reason string) bool {
	for _, exclusion := range result.Manifest.Exclusions {
		if exclusion.LogicalPath == logicalPath && exclusion.Reason == reason {
			return true
		}
	}
	return false
}

func mustWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
