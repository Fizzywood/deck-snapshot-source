package restore

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/discovery"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

type staticResolver struct {
	url     string
	hash    string
	version string
}

func (resolver staticResolver) Resolve(_ context.Context, plugins []manifest.Plugin) ([]pluginstore.Resolution, error) {
	result := make([]pluginstore.Resolution, 0, len(plugins))
	for index, plugin := range plugins {
		resolvedVersion := plugin.Version
		if resolver.version != "" {
			resolvedVersion = resolver.version
		}
		result = append(result, pluginstore.Resolution{
			SnapshotDirectory: plugin.Directory,
			SnapshotName:      plugin.Name,
			SnapshotAuthor:    plugin.Author,
			SnapshotVersion:   plugin.Version,
			Status:            "resolved",
			Message:           "Resolved by the synthetic test store.",
			StoreID:           int64(index + 1),
			StoreName:         plugin.Name,
			StoreAuthor:       plugin.Author,
			ResolvedVersion:   resolvedVersion,
			SHA256:            resolver.hash,
			ArtifactURL:       resolver.url,
		})
	}
	return result, nil
}

func TestPlanPreservesSettingsWhenStablePluginVersionChanges(t *testing.T) {
	created, _ := fixtureSnapshot(t)
	target := targetPaths(t)
	resolver := staticResolver{url: "https://example.test/package.zip", hash: strings.Repeat("a", 64), version: "2.0.0"}
	plan := buildTestPlan(t, target, created.Path, resolver, time.Date(2026, 8, 14, 14, 30, 0, 0, time.UTC))
	if len(plan.PreservedSettings) < 2 {
		t.Fatalf("version-changed plugin settings were not preserved: %#v", plan.PreservedSettings)
	}
	for _, action := range plan.Actions {
		if strings.HasPrefix(action.LogicalPath, "decky/settings/fixture-plugin/") || strings.HasPrefix(action.LogicalPath, "decky/data/fixture-plugin/") {
			t.Fatalf("incompatible setting remained a production action: %#v", action)
		}
	}
	if err := Revalidate(plan, limits.Default()); err != nil {
		t.Fatalf("preservation plan failed stale-state validation: %v", err)
	}
}

func TestBuildPlanIsReadOnlyDeterministicAndStaleAware(t *testing.T) {
	created, source := fixtureSnapshot(t)
	target := targetPaths(t)
	createTargetState(t, target, source, "decky/settings/", "old settings")
	createTargetState(t, target, source, "decky/data/", "")
	resolver := staticResolver{url: "https://example.test/package.zip", hash: strings.Repeat("a", 64)}
	before := testTreeFingerprint(t, target.Home)
	options := PlanOptions{Paths: target, SnapshotPath: created.Path, AppVersion: "phase3-test", Now: time.Date(2026, 8, 14, 14, 0, 0, 0, time.UTC), Limits: limits.Default(), Resolver: resolver}
	first, err := BuildPlan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildPlan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	after := testTreeFingerprint(t, target.Home)
	if before != after {
		t.Fatal("restore planning mutated the target tree")
	}
	if first.PlanID != second.PlanID || first.ApprovalHash != second.ApprovalHash {
		t.Fatal("fixed-input restore plans were not deterministic")
	}
	operations := map[string]bool{}
	for _, action := range first.Actions {
		operations[action.Operation] = true
	}
	if !operations["create"] || !operations["replace"] || !operations["unchanged"] || first.Blocking {
		t.Fatalf("unexpected plan operations or blocking state: %#v", operations)
	}
	planDirectory := filepath.Join(target.State, "plans")
	planPath, err := SavePlan(target.Home, planDirectory, first)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPlan(planPath)
	if err != nil || loaded.ApprovalHash != first.ApprovalHash {
		t.Fatalf("load plan: %#v %v", loaded, err)
	}
	tampered := first
	tampered.Warnings = append(tampered.Warnings, "tampered")
	if err := ValidatePlan(tampered); err == nil {
		t.Fatal("tampered plan was accepted")
	}
	for _, action := range first.Actions {
		if action.Operation == "create" {
			if err := os.MkdirAll(filepath.Dir(action.TargetPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(action.TargetPath, []byte("late collision"), 0o600); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if err := Revalidate(first, limits.Default()); err == nil {
		t.Fatal("stale target state was accepted")
	}
}

func TestBuildPlanMapsAllowlistedSteamArtworkSidecars(t *testing.T) {
	sourceHome := filepath.Join(t.TempDir(), "source-home")
	grid := filepath.Join(sourceHome, ".local", "share", "Steam", "userdata", "100000001", "config", "grid")
	if err := os.MkdirAll(grid, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grid, "900000001.json"), []byte(`{"nVersion":1,"logoPosition":{"nHeightPct":50,"nWidthPct":75.5,"pinnedPosition":"BottomCenter"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grid, "900000001_icon.ico"), []byte{0, 0, 1, 0, 1, 0, 16, 16}, 0o600); err != nil {
		t.Fatal(err)
	}
	source := platform.Paths{Home: sourceHome, Decky: filepath.Join(sourceHome, "homebrew"), Steam: filepath.Join(sourceHome, ".local", "share", "Steam")}
	result, err := discovery.Discover(context.Background(), discovery.Options{Paths: source, AppVersion: "phase3-test", DeviceID: "ds-00000000000000000000000000000000", SnapshotID: "dsnap-sidecars", Now: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC), Limits: limits.Default()})
	if err != nil {
		t.Fatal(err)
	}
	created, err := snapshot.Create(context.Background(), filepath.Join(t.TempDir(), "snapshots"), result, limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	target := targetPaths(t)
	plan := buildTestPlan(t, target, created.Path, staticResolver{}, time.Date(2026, 8, 14, 14, 0, 0, 0, time.UTC))
	expected := map[string]string{
		"steam/artwork/userdata/100000001/grid/900000001.json":     filepath.Join(target.Steam, "userdata", "100000001", "config", "grid", "900000001.json"),
		"steam/artwork/userdata/100000001/grid/900000001_icon.ico": filepath.Join(target.Steam, "userdata", "100000001", "config", "grid", "900000001_icon.ico"),
	}
	for logical, targetPath := range expected {
		found := false
		for _, action := range plan.Actions {
			if action.LogicalPath == logical {
				found = true
				if action.TargetPath != targetPath || action.Operation != "create" {
					t.Fatalf("sidecar action %s = %#v", logical, action)
				}
			}
		}
		if !found {
			t.Fatalf("sidecar was not included in the restore plan: %s", logical)
		}
	}
	if err := Revalidate(plan, limits.Default()); err != nil {
		t.Fatalf("sidecar restore plan did not revalidate: %v", err)
	}
}

func TestMoveNoReplaceNeverOverwritesDestination(t *testing.T) {
	home := t.TempDir()
	directory := filepath.Join(home, "files")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "source")
	destination := filepath.Join(directory, "destination")
	if err := os.WriteFile(source, []byte("source-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("destination-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveNoReplace(home, source, destination, false); err == nil {
		t.Fatal("moveNoReplace overwrote an existing destination")
	}
	sourceData, sourceErr := os.ReadFile(source)
	destinationData, destinationErr := os.ReadFile(destination)
	if sourceErr != nil || destinationErr != nil || string(sourceData) != "source-data" || string(destinationData) != "destination-data" {
		t.Fatalf("no-replace collision changed content: source=%q destination=%q sourceErr=%v destinationErr=%v", sourceData, destinationData, sourceErr, destinationErr)
	}
}

func TestPluginRollbackPreservesUnexpectedConcurrentContent(t *testing.T) {
	target := targetPaths(t)
	pluginRoot := filepath.Join(target.Decky, "plugins")
	pluginPath := filepath.Join(pluginRoot, "concurrent-plugin")
	if err := os.MkdirAll(pluginPath, 0o700); err != nil {
		t.Fatal(err)
	}
	contentPath := filepath.Join(pluginPath, "unexpected.txt")
	if err := os.WriteFile(contentPath, []byte("concurrent content"), 0o600); err != nil {
		t.Fatal(err)
	}
	item := installedPlugin{
		Action:          PluginAction{Directory: "concurrent-plugin", TargetRoot: pluginRoot, TargetPath: pluginPath, Operation: "create"},
		NewFingerprint:  strings.Repeat("0", 64),
		NewFiles:        1,
		NewBytes:        int64(len("concurrent content")),
		MutationStarted: true,
	}
	if err := rollbackInstalledPlugins(target.Home, []installedPlugin{item}, limits.Default()); err == nil {
		t.Fatal("rollback accepted unexpected concurrent plugin content")
	}
	contents, err := os.ReadFile(contentPath)
	if err != nil || string(contents) != "concurrent content" {
		t.Fatalf("rollback lost concurrent plugin content: %q %v", contents, err)
	}
	transactions, err := filepath.Glob(filepath.Join(pluginRoot, ".deck-snapshot-plugin-rollback-*"))
	if err != nil || len(transactions) != 1 {
		t.Fatalf("uncertain rollback transaction was not retained: %v %#v", err, transactions)
	}
}

func TestBuildPlanRejectsSymlinkedStateRoot(t *testing.T) {
	created, _ := fixtureSnapshot(t)
	target := targetPaths(t)
	if err := os.MkdirAll(filepath.Dir(target.State), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, target.State); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	resolver := staticResolver{url: "https://example.test/package.zip", hash: strings.Repeat("a", 64)}
	_, err := BuildPlan(context.Background(), PlanOptions{Paths: target, SnapshotPath: created.Path, AppVersion: "phase3-test", Now: time.Now(), Limits: limits.Default(), Resolver: resolver})
	if err == nil {
		t.Fatal("BuildPlan accepted a symlinked application state root")
	}
}

func TestRunRestoresFixtureAndIsIdempotent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("crash-safe transactional restore requires Linux rename exchange")
	}
	archive := fixturePluginZIP(t)
	hash := sha256.Sum256(archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/zip")
		response.Write(archive)
	}))
	defer server.Close()
	created, source := fixtureSnapshot(t)
	target := targetPaths(t)
	createTargetState(t, target, source, "decky/settings/", "outdated settings")
	resolver := staticResolver{url: server.URL + "/fixture.zip", hash: hex.EncodeToString(hash[:])}
	plan := buildTestPlan(t, target, created.Path, resolver, time.Date(2026, 8, 14, 15, 0, 0, 0, time.UTC))
	report, err := Run(context.Background(), testRunOptions(target, plan, server.Client()))
	if err != nil || report.Status != "succeeded" || report.RecoverySnapshotPath == "" || report.ReportPath == "" {
		t.Fatalf("Run() report=%#v error=%v", report, err)
	}
	if _, err := snapshot.Validate(report.RecoverySnapshotPath, limits.Default()); err != nil {
		t.Fatalf("recovery snapshot is invalid: %v", err)
	}
	reportBytes, err := os.ReadFile(report.ReportPath)
	var persisted Report
	if err == nil {
		err = json.Unmarshal(reportBytes, &persisted)
	}
	if err != nil || persisted.RecoverySnapshotPath != report.RecoverySnapshotPath || persisted.ReportPath != report.ReportPath {
		t.Fatalf("persisted report does not retain exact recovery/report paths: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target.Decky, "plugins", "fixture-plugin", "plugin.json")); err != nil {
		t.Fatalf("plugin was not installed: %v", err)
	}
	rediscovered := discoverTarget(t, target)
	if len(rediscovered.Manifest.Plugins) != len(source.Manifest.Plugins) || rediscovered.Manifest.Plugins[0].Directory != source.Manifest.Plugins[0].Directory {
		t.Fatalf("plugin rediscovery mismatch: %#v", rediscovered.Manifest.Plugins)
	}
	if fmt.Sprint(payloadInventory(rediscovered.Manifest.Files)) != fmt.Sprint(payloadInventory(source.Manifest.Files)) {
		t.Fatalf("restored payload inventory differs\nwant: %v\n got: %v", payloadInventory(source.Manifest.Files), payloadInventory(rediscovered.Manifest.Files))
	}

	secondPlan := buildTestPlan(t, target, created.Path, resolver, time.Date(2026, 8, 14, 15, 1, 0, 0, time.UTC))
	for _, action := range secondPlan.Actions {
		if action.Operation != "unchanged" {
			t.Fatalf("second file restore is not idempotent: %#v", action)
		}
	}
	if len(secondPlan.PluginActions) != 1 || secondPlan.PluginActions[0].Operation != "unchanged" {
		t.Fatalf("second plugin restore is not idempotent: %#v", secondPlan.PluginActions)
	}
	secondReport, err := Run(context.Background(), testRunOptions(target, secondPlan, server.Client()))
	if err != nil || secondReport.Status != "succeeded" {
		t.Fatalf("second Run() report=%#v error=%v", secondReport, err)
	}
}

func TestRunRollsBackFilesAndPluginsAfterInjectedFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("crash-safe transactional restore requires Linux rename exchange")
	}
	archive := fixturePluginZIP(t)
	hash := sha256.Sum256(archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) { response.Write(archive) }))
	defer server.Close()
	created, _ := fixtureSnapshot(t)
	target := targetPaths(t)
	oldPlugin := filepath.Join(target.Decky, "plugins", "fixture-plugin")
	if err := os.MkdirAll(oldPlugin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPlugin, "plugin.json"), []byte(`{"name":"Fixture Plugin","author":"Deck Snapshot Tests"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPlugin, "package.json"), []byte(`{"name":"fixture-plugin","version":"0.9.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPlugin, "legacy.txt"), []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldFingerprint, _, _, err := fingerprintPluginTree(oldPlugin, limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	resolver := staticResolver{url: server.URL + "/fixture.zip", hash: hex.EncodeToString(hash[:])}
	plan := buildTestPlan(t, target, created.Path, resolver, time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC))
	options := testRunOptions(target, plan, server.Client())
	count := 0
	options.BeforeAction = func(Action) error {
		count++
		if count == 2 {
			return errors.New("injected action failure")
		}
		return nil
	}
	report, err := Run(context.Background(), options)
	if err == nil || report.Status != "rolled_back" {
		t.Fatalf("failed run did not roll back: %#v %v", report, err)
	}
	restoredFingerprint, _, _, fingerprintErr := fingerprintPluginTree(oldPlugin, limits.Default())
	if fingerprintErr != nil || restoredFingerprint != oldFingerprint {
		t.Fatalf("replaced plugin was not restored after rollback: %s %s %v", oldFingerprint, restoredFingerprint, fingerprintErr)
	}
	firstApplied := plan.Actions[0]
	if _, err := os.Lstat(firstApplied.TargetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created file remained after rollback: %v", err)
	}
	if _, err := snapshot.Validate(report.RecoverySnapshotPath, limits.Default()); err != nil {
		t.Fatalf("recovery snapshot was not retained: %v", err)
	}
}

func TestRunFailsBeforeMutationWhenRecoveryCannotBeCreated(t *testing.T) {
	archive := fixturePluginZIP(t)
	hash := sha256.Sum256(archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) { response.Write(archive) }))
	defer server.Close()
	created, _ := fixtureSnapshot(t)
	target := targetPaths(t)
	resolver := staticResolver{url: server.URL, hash: hex.EncodeToString(hash[:])}
	plan := buildTestPlan(t, target, created.Path, resolver, time.Date(2026, 8, 14, 16, 30, 0, 0, time.UTC))
	options := testRunOptions(target, plan, server.Client())
	if err := os.MkdirAll(filepath.Dir(options.RecoveryDirectory), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.RecoveryDirectory, []byte("collision"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), options)
	if err == nil || report.Status != "failed_before_mutation" {
		t.Fatalf("recovery failure was not closed before mutation: %#v %v", report, err)
	}
	for _, action := range plan.Actions {
		if _, err := os.Lstat(action.TargetPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("target was mutated before recovery: %q %v", action.TargetPath, err)
		}
	}
	if _, err := os.Lstat(plan.PluginActions[0].TargetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plugin target was mutated before recovery: %v", err)
	}
}

func TestValidateStagedRecoveryRejectsReusedSnapshotID(t *testing.T) {
	const (
		logicalPath  = "decky/settings/fixture-plugin/settings.json"
		originalHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		foreignHash  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	plan := Plan{Actions: []Action{{
		LogicalPath:    logicalPath,
		Operation:      "replace",
		ExistingSize:   12,
		ExistingSHA256: originalHash,
		ExistingMode:   0o600,
	}}}
	expected := manifest.New("recovery-plan", "phase3-test", "recovery", "Restore recovery", time.Date(2026, 8, 14, 16, 0, 0, 0, time.UTC))
	expected.Files = []manifest.File{{LogicalPath: logicalPath, Component: "recovery/decky/settings", Size: 12, SHA256: originalHash, Mode: 0o600}}
	expected.Normalize()

	validStaged := map[string]snapshot.StagedFile{logicalPath: {LogicalPath: logicalPath, Path: filepath.Join(t.TempDir(), "payload"), Size: 12, SHA256: originalHash}}
	if err := validateStagedRecovery(plan, expected, expected, validStaged); err != nil {
		t.Fatalf("validateStagedRecovery() rejected exact recovery: %v", err)
	}

	t.Run("different checksum", func(t *testing.T) {
		foreign := expected
		foreign.Files = append([]manifest.File(nil), expected.Files...)
		foreign.Files[0].SHA256 = foreignHash
		foreignStaged := map[string]snapshot.StagedFile{logicalPath: {LogicalPath: logicalPath, Path: filepath.Join(t.TempDir(), "payload"), Size: 12, SHA256: foreignHash}}
		if err := validateStagedRecovery(plan, expected, foreign, foreignStaged); err == nil {
			t.Fatal("validateStagedRecovery() accepted a reused snapshot ID with a different checksum")
		}
	})

	t.Run("different inventory", func(t *testing.T) {
		foreign := expected
		foreign.Files = append([]manifest.File(nil), expected.Files...)
		foreign.Files = append(foreign.Files, manifest.File{LogicalPath: "decky/data/foreign/file.json", Component: "recovery/decky/data", Size: 1, SHA256: foreignHash, Mode: 0o600})
		foreign.Normalize()
		if err := validateStagedRecovery(plan, expected, foreign, validStaged); err == nil {
			t.Fatal("validateStagedRecovery() accepted a reused snapshot ID with a different inventory")
		}
	})

	t.Run("different valid archive", func(t *testing.T) {
		createArchive := func(t *testing.T, root string, content []byte) snapshot.Created {
			t.Helper()
			sourcePath := filepath.Join(root, "source.json")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content)
			value := manifest.New("recovery-same-plan-id", "phase3-test", "recovery", "Restore recovery", time.Date(2026, 8, 14, 16, 30, 0, 0, time.UTC))
			entry := manifest.File{LogicalPath: logicalPath, Component: "recovery/decky/settings", Size: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), Mode: 0o600}
			value.Files = []manifest.File{entry}
			value.Normalize()
			created, err := snapshot.Create(context.Background(), filepath.Join(root, "snapshots"), discovery.Result{Manifest: value, Candidates: []discovery.Candidate{{SourcePath: sourcePath, Entry: entry}}}, limits.Default())
			if err != nil {
				t.Fatal(err)
			}
			return created
		}

		expectedArchive := createArchive(t, filepath.Join(t.TempDir(), "expected"), []byte("approved recovery bytes"))
		foreignArchive := createArchive(t, filepath.Join(t.TempDir(), "foreign"), []byte("different recovery bytes"))
		stageDirectory := filepath.Join(t.TempDir(), "stage")
		if err := os.Mkdir(stageDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		foreignStaged, foreignManifest, err := snapshot.StageSelected(context.Background(), foreignArchive.Path, stageDirectory, []string{logicalPath}, limits.Default())
		if err != nil {
			t.Fatalf("StageSelected() rejected independently valid foreign recovery: %v", err)
		}
		expectedEntry := expectedArchive.Manifest.Files[0]
		archivePlan := Plan{Actions: []Action{{LogicalPath: logicalPath, Operation: "replace", ExistingSize: expectedEntry.Size, ExistingSHA256: expectedEntry.SHA256, ExistingMode: expectedEntry.Mode}}}
		if err := validateStagedRecovery(archivePlan, expectedArchive.Manifest, foreignManifest, foreignStaged); err == nil {
			t.Fatal("validateStagedRecovery() accepted a different valid recovery archive reusing the same snapshot ID")
		}
	})
}

func TestRunRejectsInsufficientSpaceAndExactApprovalMismatch(t *testing.T) {
	created, _ := fixtureSnapshot(t)
	target := targetPaths(t)
	resolver := staticResolver{url: "https://example.test/package.zip", hash: strings.Repeat("a", 64)}
	plan := buildTestPlan(t, target, created.Path, resolver, time.Date(2026, 8, 14, 16, 40, 0, 0, time.UTC))
	options := testRunOptions(target, plan, nil)
	options.ApprovedHash = strings.Repeat("0", 64)
	if _, err := Run(context.Background(), options); err == nil {
		t.Fatal("mismatched approval hash was accepted")
	}
	options.ApprovedHash = plan.ApprovalHash
	options.AvailableBytes = func(string) (uint64, error) { return 1, nil }
	if _, err := Run(context.Background(), options); err == nil {
		t.Fatal("insufficient space was accepted")
	}
	if _, err := os.Lstat(plan.PluginActions[0].TargetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target was mutated after a preflight failure: %v", err)
	}
}

func TestValidateTargetRejectsEscapesAndSymlinkParents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cases := [][2]string{{"relative", filepath.Join(root, "file")}, {root, filepath.Join(root, "..", "escape")}, {root, root}}
	for _, pair := range cases {
		if err := ValidateTarget(pair[0], pair[1]); err == nil {
			t.Fatalf("unsafe target accepted: %q %q", pair[0], pair[1])
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.Mkdir(outside, 0o700)
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err == nil {
		if err := ValidateTarget(root, filepath.Join(link, "file")); err == nil {
			t.Fatal("symlink parent was accepted")
		}
	}
}

func fixtureSnapshot(t *testing.T) (snapshot.Created, discovery.Result) {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join("..", "..", "tests", "fixtures", "deck-home"))
	if err != nil {
		t.Fatal(err)
	}
	paths := platform.Paths{Home: fixture, Decky: filepath.Join(fixture, "homebrew"), Steam: filepath.Join(fixture, ".local", "share", "Steam")}
	result, err := discovery.Discover(context.Background(), discovery.Options{Paths: paths, AppVersion: "phase3-test", DeviceID: "ds-00000000000000000000000000000000", SnapshotID: "dsnap-phase3fixture", Now: time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC), Limits: limits.Default()})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "snapshots")
	created, err := snapshot.Create(context.Background(), directory, result, limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	return created, result
}

func targetPaths(t *testing.T) platform.Paths {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return platform.Paths{Home: home, Decky: filepath.Join(home, "homebrew"), Steam: filepath.Join(home, ".local", "share", "Steam"), State: filepath.Join(home, ".local", "state", "deck-snapshot")}
}

func buildTestPlan(t *testing.T, target platform.Paths, snapshotPath string, resolver staticResolver, now time.Time) Plan {
	t.Helper()
	plan, err := BuildPlan(context.Background(), PlanOptions{Paths: target, SnapshotPath: snapshotPath, AppVersion: "phase3-test", Now: now, Limits: limits.Default(), Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Blocking {
		t.Fatalf("test plan unexpectedly blocked: %#v %#v", plan.Actions, plan.PluginActions)
	}
	return plan
}

func testRunOptions(target platform.Paths, plan Plan, client *http.Client) RunOptions {
	return RunOptions{
		Plan: plan, ApprovedPlanID: plan.PlanID, ApprovedHash: plan.ApprovalHash, Limits: limits.Default(),
		WorkDirectory: filepath.Join(target.State, "work"), RecoveryDirectory: filepath.Join(target.State, "recovery"), ReportDirectory: filepath.Join(target.State, "reports"),
		Now: func() time.Time { return time.Date(2026, 8, 14, 17, 0, 0, 0, time.UTC) }, AvailableBytes: func(string) (uint64, error) { return math.MaxUint64, nil }, HTTPClient: client,
	}
}

func createTargetState(t *testing.T, target platform.Paths, source discovery.Result, prefix, replacement string) {
	t.Helper()
	for _, candidate := range source.Candidates {
		if !strings.HasPrefix(candidate.Entry.LogicalPath, prefix) || candidate.Entry.Generated {
			continue
		}
		_, targetPath, mapped, err := mapTarget(target, candidate.Entry.LogicalPath)
		if err != nil || !mapped {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(candidate.SourcePath)
		if err != nil {
			t.Fatal(err)
		}
		if replacement != "" {
			contents = []byte(replacement)
		}
		if err := os.WriteFile(targetPath, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("no source candidate matched %q", prefix)
}

func discoverTarget(t *testing.T, paths platform.Paths) discovery.Result {
	t.Helper()
	result, err := discovery.Discover(context.Background(), discovery.Options{Paths: paths, AppVersion: "phase3-test", DeviceID: "ds-11111111111111111111111111111111", SnapshotID: "dsnap-rediscovered", Now: time.Date(2026, 8, 14, 18, 0, 0, 0, time.UTC), Limits: limits.Default()})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func payloadInventory(files []manifest.File) []string {
	result := make([]string, 0, len(files))
	for _, file := range files {
		if strings.HasPrefix(file.LogicalPath, "reports/") {
			continue
		}
		result = append(result, file.LogicalPath+"="+file.SHA256)
	}
	sort.Strings(result)
	return result
}

func fixturePluginZIP(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entries := map[string]string{
		"fixture-plugin/plugin.json":   `{"name":"Fixture Plugin","author":"Deck Snapshot Tests","api_version":1}`,
		"fixture-plugin/package.json":  `{"name":"fixture-plugin","version":"1.2.3","private":true}`,
		"fixture-plugin/dist/index.js": "console.log('fixture plugin');\n",
	}
	keys := make([]string, 0, len(entries))
	for name := range entries {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		entry.Write([]byte(entries[name]))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testTreeFingerprint(t *testing.T, root string) string {
	t.Helper()
	hash := sha256.New()
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, current)
		fmt.Fprintf(hash, "%s\x00%s\n", filepath.ToSlash(relative), entry.Type())
		if entry.Type().IsRegular() {
			contents, err := os.ReadFile(current)
			if err != nil {
				return err
			}
			hash.Write(contents)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
