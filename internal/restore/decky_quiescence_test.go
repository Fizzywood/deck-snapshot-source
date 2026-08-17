package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
)

type fakeDeckyController struct {
	inventory    []deckyapi.PluginStatus
	disabled     []string
	restarts     int
	setErr       error
	restartErr   error
	inventoryErr error
	disabledErr  error
}

type markerRebooter struct{ boot string }

func (rebooter markerRebooter) Preflight(context.Context) error        { return nil }
func (rebooter markerRebooter) Request(context.Context) error          { return nil }
func (rebooter markerRebooter) BootID(context.Context) (string, error) { return rebooter.boot, nil }

func (fake *fakeDeckyController) Probe(context.Context, string) error                    { return nil }
func (fake *fakeDeckyController) Install(context.Context, deckyapi.InstallRequest) error { return nil }
func (fake *fakeDeckyController) Uninstall(context.Context, string) error                { return nil }
func (fake *fakeDeckyController) Restart(context.Context, string) error {
	fake.restarts++
	return fake.restartErr
}
func (fake *fakeDeckyController) Inventory(context.Context) ([]deckyapi.PluginStatus, error) {
	if fake.inventoryErr != nil {
		return nil, fake.inventoryErr
	}
	return append([]deckyapi.PluginStatus(nil), fake.inventory...), nil
}
func (fake *fakeDeckyController) DisabledPlugins(context.Context) ([]string, error) {
	if fake.disabledErr != nil {
		return nil, fake.disabledErr
	}
	return append([]string(nil), fake.disabled...), nil
}
func (fake *fakeDeckyController) SetDisabledPlugins(_ context.Context, names []string) error {
	if fake.setErr != nil {
		return fake.setErr
	}
	fake.disabled = append([]string(nil), names...)
	for index := range fake.inventory {
		fake.inventory[index].Disabled = containsPluginName(names, fake.inventory[index].Name)
	}
	return nil
}

func TestDeckyQuiescenceDisablesEveryPluginBeforeRestart(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1"}, {Name: "Alarm Me", Version: "1"}}, disabled: []string{"Already Disabled"}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	state, err := coordinator.PlanQuiescence(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")})
	if err != nil {
		t.Fatal(err)
	}
	if !equalPluginNames(state.TemporaryDisabled, []string{"Alarm Me", "Already Disabled", "CSS Loader"}) {
		t.Fatalf("temporary disabled state = %#v", state.TemporaryDisabled)
	}
	if err := coordinator.Quiesce(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")}, state); err != nil {
		t.Fatal(err)
	}
	if controller.restarts != 1 {
		t.Fatalf("restart count = %d", controller.restarts)
	}
	if err := coordinator.VerifyQuiescent(context.Background(), state, []string{"Alarm Me", "CSS Loader"}); err != nil {
		t.Fatal(err)
	}
}

func TestDeckyQuiescenceRejectsActivePlugin(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1", Disabled: true}}, disabled: []string{"CSS Loader"}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	state, err := coordinator.PlanQuiescence(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")})
	if err != nil {
		t.Fatal(err)
	}
	controller.inventory[0].Disabled = false
	if err := coordinator.VerifyQuiescent(context.Background(), state, []string{"CSS Loader"}); err == nil {
		t.Fatal("VerifyQuiescent accepted an active plugin")
	}
}

func TestDeckyQuiescenceAllowsSafeStaleDisabledIdentity(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1", Disabled: true}}, disabled: []string{"CSS Loader", "Former Plugin"}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	if err := coordinator.VerifyQuiescent(context.Background(), Quiescence{TemporaryDisabled: []string{"CSS Loader"}}, []string{"CSS Loader"}); err != nil {
		t.Fatalf("VerifyQuiescent() rejected safe stale identity: %v", err)
	}
}

func TestDeckyQuiescenceRejectsMissingCurrentDisabledIdentity(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1", Disabled: true}}, disabled: []string{"Former Plugin"}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	if err := coordinator.VerifyQuiescent(context.Background(), Quiescence{}, []string{"CSS Loader"}); err == nil {
		t.Fatal("VerifyQuiescent accepted a current plugin missing from disabled_plugins")
	}
}

func TestDeckyQuiescenceRejectsMalformedStaleDisabledIdentity(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1", Disabled: true}}, disabled: []string{"CSS Loader", "unsafe\nidentity"}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	if err := coordinator.VerifyQuiescent(context.Background(), Quiescence{}, []string{"CSS Loader"}); err == nil {
		t.Fatal("VerifyQuiescent accepted an unsafe stale disabled identity")
	}
}

func TestDeckyQuiescenceSynchronizesAfterDeckyUninstall(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "Alarm Me", Version: "1"}, {Name: "CSS Loader", Version: "1"}}, disabled: []string{"Former Plugin"}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	state, err := coordinator.PlanQuiescence(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Quiesce(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")}, state); err != nil {
		t.Fatal(err)
	}
	controller.inventory = []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1", Disabled: true}}
	controller.disabled = []string{"CSS Loader", "Former Plugin"}
	if err := coordinator.SynchronizeQuiescence(context.Background(), &state, []string{"CSS Loader"}); err != nil {
		t.Fatal(err)
	}
	if !equalPluginNames(state.TemporaryDisabled, []string{"CSS Loader", "Former Plugin"}) {
		t.Fatalf("synchronized temporary state = %#v", state.TemporaryDisabled)
	}
	if !equalPluginNames(state.OriginalDisabled, []string{"Former Plugin"}) {
		t.Fatalf("original disabled state changed = %#v", state.OriginalDisabled)
	}
}

func TestDeckyQuiescenceSynchronizesAfterDeckyInstall(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1"}}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	state, err := coordinator.PlanQuiescence(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Quiesce(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")}, state); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.KeepDisabled(context.Background(), &state, "New Plugin"); err != nil {
		t.Fatal(err)
	}
	controller.inventory = append(controller.inventory, deckyapi.PluginStatus{Name: "New Plugin", Version: "1", Disabled: true})
	if err := coordinator.SynchronizeQuiescence(context.Background(), &state, []string{"CSS Loader", "New Plugin"}); err != nil {
		t.Fatal(err)
	}
	if !equalPluginNames(state.TemporaryDisabled, []string{"CSS Loader", "New Plugin"}) {
		t.Fatalf("synchronized temporary state = %#v", state.TemporaryDisabled)
	}
}

func TestDeckyQuiescenceOriginalStateCanBeRestoredExactly(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1"}}, disabled: []string{"Former Plugin"}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	state, err := coordinator.PlanQuiescence(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Quiesce(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")}, state); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RestoreOriginal(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")}, state); err != nil {
		t.Fatal(err)
	}
	if !equalPluginNames(controller.disabled, []string{"Former Plugin"}) {
		t.Fatalf("restored original disabled state = %#v", controller.disabled)
	}
}

func TestDeckyQuiescenceRestoresEmptyOriginalStateExactly(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "Alarm Me", Version: "1"}, {Name: "CSS Loader", Version: "1"}}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	target := TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")}
	state, err := coordinator.PlanQuiescence(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Quiesce(context.Background(), target, state); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RestoreOriginal(context.Background(), target, state); err != nil {
		t.Fatal(err)
	}
	if len(controller.disabled) != 0 {
		t.Fatalf("restored disabled state = %#v, want empty", controller.disabled)
	}
	for _, plugin := range controller.inventory {
		if plugin.Disabled {
			t.Fatalf("plugin %q remained disabled", plugin.Name)
		}
	}
}

func TestDeckyQuiescenceRestoresOriginalInventoryAfterPluginRollback(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "Alarm Me", Version: "1"}, {Name: "CSS Loader", Version: "1"}}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	target := TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")}
	state, err := coordinator.PlanQuiescence(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Quiesce(context.Background(), target, state); err != nil {
		t.Fatal(err)
	}
	// Decky removes Alarm Me while it is uninstalled. The bounded plugin rollback
	// reinstalls it before original-state cleanup, but the temporary inventory
	// bookkeeping remains the post-uninstall state.
	state.Inventory = removePluginStatus(state.Inventory, "Alarm Me")
	controller.inventory = []deckyapi.PluginStatus{{Name: "Alarm Me", Version: "1", Disabled: true}, {Name: "CSS Loader", Version: "1", Disabled: true}}
	controller.disabled = []string{"Alarm Me", "CSS Loader"}
	if err := coordinator.RestoreOriginal(context.Background(), target, state); err != nil {
		t.Fatal(err)
	}
	if len(controller.disabled) != 0 || controller.inventory[0].Disabled || controller.inventory[1].Disabled {
		t.Fatalf("original plugin state was not restored: disabled=%#v inventory=%#v", controller.disabled, controller.inventory)
	}
}

func TestDeckyQuiescenceRestoresLegitimateNonEmptyOriginalStateExactly(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1", Disabled: true}, {Name: "SteamGridDB", Version: "1"}}, disabled: []string{"CSS Loader", "Former Plugin"}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	target := TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")}
	state, err := coordinator.PlanQuiescence(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Quiesce(context.Background(), target, state); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RestoreOriginal(context.Background(), target, state); err != nil {
		t.Fatal(err)
	}
	if !equalPluginNames(controller.disabled, []string{"CSS Loader", "Former Plugin"}) {
		t.Fatalf("restored disabled state = %#v", controller.disabled)
	}
	if !controller.inventory[0].Disabled || controller.inventory[1].Disabled {
		t.Fatalf("effective state was not restored: %#v", controller.inventory)
	}
}

func TestDeckyQuiescenceRestoreOriginalFailsClosedOnRestartFailure(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1"}}, restartErr: errors.New("restart failed")}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	target := TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")}
	state, err := coordinator.PlanQuiescence(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	controller.restartErr = nil
	if err := coordinator.Quiesce(context.Background(), target, state); err != nil {
		t.Fatal(err)
	}
	controller.restartErr = errors.New("restart failed")
	if err := coordinator.RestoreOriginal(context.Background(), target, state); err == nil {
		t.Fatal("RestoreOriginal accepted a failed Decky restart")
	}
}

func TestDeckyQuiescenceRestoreOriginalFailsClosedWhenStateCannotBeVerified(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1"}}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	target := TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")}
	state, err := coordinator.PlanQuiescence(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Quiesce(context.Background(), target, state); err != nil {
		t.Fatal(err)
	}
	controller.disabledErr = errors.New("read failed")
	if err := coordinator.RestoreOriginal(context.Background(), target, state); err == nil {
		t.Fatal("RestoreOriginal accepted an unverifiable disabled state")
	}
}

func TestIncompleteTransactionMarkerBlocksSecondTransaction(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".local", "state", "deck-snapshot")
	marker := incompleteTransactionMarker{Schema: 2, PlanID: "restore-abc", SnapshotID: "dsnap-abc", SnapshotPath: filepath.Join(home, "snapshot.tar.gz"), RecoveryPath: filepath.Join(state, "recovery", "snapshot.tar.gz"), OriginalDisabledPlugins: []string{}, TemporaryDisabledPlugins: []string{"CSS Loader"}, PreRebootBootID: "00000000-0000-0000-0000-000000000000", Phase: "prepared"}
	if err := saveIncompleteTransaction(home, state, marker); err != nil {
		t.Fatal(err)
	}
	if err := saveIncompleteTransaction(home, state, marker); err == nil {
		t.Fatal("second marker write unexpectedly replaced the first")
	}
	loaded, pending, err := loadIncompleteTransaction(home, state)
	if err != nil || !pending || loaded.PlanID != marker.PlanID {
		t.Fatalf("load marker = %#v, %v, %v", loaded, pending, err)
	}
	if err := removeIncompleteTransaction(home, state); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := loadIncompleteTransaction(home, state); err != nil || pending {
		t.Fatalf("marker remained after removal: %v %v", pending, err)
	}
}

func TestIncompleteTransactionMarkerRejectsLegacySchema(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(home, ".local", "state", "deck-snapshot")
	marker := incompleteTransactionMarker{Schema: 1, PlanID: "restore-abc", SnapshotID: "dsnap-abc", SnapshotPath: filepath.Join(home, "snapshot.tar.gz"), RecoveryPath: filepath.Join(state, "recovery", "snapshot.tar.gz"), PreRebootBootID: "00000000-0000-0000-0000-000000000000", Phase: "prepared"}
	if err := saveIncompleteTransaction(home, state, marker); err == nil {
		t.Fatal("legacy transaction marker schema was accepted")
	}
	if err := ensureSecureDirectory(home, state); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"schema":1,"plan_id":"restore-abc","snapshot_id":"dsnap-abc","snapshot_path":"` + filepath.ToSlash(filepath.Join(home, "snapshot.tar.gz")) + `","recovery_path":"` + filepath.ToSlash(filepath.Join(state, "recovery", "snapshot.tar.gz")) + `","disabled_plugins":[],"pre_reboot_boot_id":"00000000-0000-0000-0000-000000000000","phase":"prepared"}`)
	if err := os.WriteFile(transactionMarkerPath(state), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := loadIncompleteTransaction(home, state); err == nil || pending {
		t.Fatalf("legacy transaction marker was silently accepted: pending=%v error=%v", pending, err)
	}
}

func TestQuiescenceRejectsUnexpectedInventory(t *testing.T) {
	controller := &fakeDeckyController{inventory: []deckyapi.PluginStatus{{Name: "CSS Loader", Version: "1", Disabled: true}}, disabled: []string{"CSS Loader"}}
	coordinator := NewDeckyRuntimeCoordinator(controller).(QuiescenceCoordinator)
	state, err := coordinator.PlanQuiescence(context.Background(), TargetReference{Decky: filepath.Join(t.TempDir(), "homebrew")})
	if err != nil {
		t.Fatal(err)
	}
	controller.inventory = append(controller.inventory, deckyapi.PluginStatus{Name: "Unexpected", Version: "1", Disabled: true})
	if err := coordinator.VerifyQuiescent(context.Background(), state, []string{"CSS Loader"}); err == nil {
		t.Fatal("unexpected inventory was accepted")
	}
}

func TestPlanMutatesSteamOnlyForChangingArtworkActions(t *testing.T) {
	if planMutatesSteam(Plan{Actions: []Action{{Component: "steam", Operation: "unchanged"}}}) {
		t.Fatal("unchanged Steam state required quiescence")
	}
	if !planMutatesSteam(Plan{Actions: []Action{{Component: "steam", Operation: "replace"}}}) {
		t.Fatal("changing Steam state did not require quiescence")
	}
}

func TestPendingRestoreWithoutMarkerContinuesNormally(t *testing.T) {
	home := t.TempDir()
	paths := platform.Paths{Home: home, State: filepath.Join(home, ".local", "state", "deck-snapshot")}
	state, err := CheckPendingRestore(context.Background(), paths, "test", nil, nil, nil)
	if err != nil || state != PendingRestoreNone {
		t.Fatalf("CheckPendingRestore() = %q, %v", state, err)
	}
}

func TestPendingRestoreSameBootRequiresRestart(t *testing.T) {
	home := t.TempDir()
	stateRoot := filepath.Join(home, ".local", "state", "deck-snapshot")
	marker := incompleteTransactionMarker{Schema: 2, PlanID: "restore-abc", SnapshotID: "dsnap-abc", SnapshotPath: filepath.Join(home, "snapshot.tar.gz"), RecoveryPath: filepath.Join(stateRoot, "recovery", "snapshot.tar.gz"), OriginalDisabledPlugins: []string{}, TemporaryDisabledPlugins: []string{"CSS Loader"}, PreRebootBootID: "00000000-0000-0000-0000-000000000000", Phase: "awaiting_reboot"}
	if err := saveIncompleteTransaction(home, stateRoot, marker); err != nil {
		t.Fatal(err)
	}
	paths := platform.Paths{Home: home, State: stateRoot}
	state, err := CheckPendingRestore(context.Background(), paths, "test", nil, nil, markerRebooter{boot: marker.PreRebootBootID})
	if err != nil || state != PendingRestoreRestartNeeded {
		t.Fatalf("CheckPendingRestore() = %q, %v", state, err)
	}
}
