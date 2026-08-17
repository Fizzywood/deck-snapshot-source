package restore

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
)

// Quiescence is the bounded record of the Decky state proved before direct
// filesystem convergence. It intentionally contains display identities only.
type Quiescence struct {
	OriginalDisabled  []string
	TemporaryDisabled []string
	OriginalInventory []deckyapi.PluginStatus
	Inventory         []deckyapi.PluginStatus
}

// QuiescenceCoordinator is deliberately not a general RPC interface. Its only
// purpose is to hold every Decky plugin backend, including CSS Loader, stopped
// while the already-sealed filesystem plan is applied.
type QuiescenceCoordinator interface {
	PlanQuiescence(context.Context, TargetReference) (Quiescence, error)
	Quiesce(context.Context, TargetReference, Quiescence) error
	KeepDisabled(context.Context, *Quiescence, string) error
	VerifyQuiescent(context.Context, Quiescence, []string) error
	SynchronizeQuiescence(context.Context, *Quiescence, []string) error
	RestoreOriginal(context.Context, TargetReference, Quiescence) error
}

// SteamQuiescer is intentionally separate from the Decky control contract.
// It has one fixed operation: gracefully stop Steam and prove its supported
// artwork writers are no longer present before artwork files are changed.
type SteamQuiescer interface {
	QuiesceSteam(context.Context) error
}

func (coordinator deckyRuntimeCoordinator) PlanQuiescence(ctx context.Context, target TargetReference) (Quiescence, error) {
	if err := coordinator.controller.Probe(ctx, target.Decky); err != nil {
		return Quiescence{}, fmt.Errorf("verify supported Decky contract: %w", err)
	}
	inventory, err := coordinator.controller.Inventory(ctx)
	if err != nil {
		return Quiescence{}, fmt.Errorf("read Decky plugin inventory: %w", err)
	}
	disabled, err := coordinator.controller.DisabledPlugins(ctx)
	if err != nil {
		return Quiescence{}, fmt.Errorf("read Decky disabled-plugin state: %w", err)
	}
	disabled, err = canonicalPluginNames(disabled)
	if err != nil {
		return Quiescence{}, fmt.Errorf("validate Decky disabled-plugin state: %w", err)
	}
	temporary := append([]string(nil), disabled...)
	for _, plugin := range inventory {
		temporary = append(temporary, plugin.Name)
	}
	temporary, err = canonicalPluginNames(temporary)
	if err != nil {
		return Quiescence{}, err
	}
	state := Quiescence{OriginalDisabled: disabled, TemporaryDisabled: temporary, OriginalInventory: append([]deckyapi.PluginStatus(nil), inventory...), Inventory: inventory}
	if err := coordinator.verifyOriginalState(ctx, state); err != nil {
		return Quiescence{}, fmt.Errorf("verify original Decky plugin state: %w", err)
	}
	return state, nil
}

func (coordinator deckyRuntimeCoordinator) Quiesce(ctx context.Context, target TargetReference, state Quiescence) error {
	if err := coordinator.controller.SetDisabledPlugins(ctx, state.TemporaryDisabled); err != nil {
		return fmt.Errorf("persist temporary disabled-plugin state: %w", err)
	}
	if err := coordinator.restarter.Restart(ctx, target.Decky); err != nil {
		return fmt.Errorf("restart Decky into its disabled-plugin state: %w", err)
	}
	names := make([]string, 0, len(state.Inventory))
	for _, plugin := range state.Inventory {
		names = append(names, plugin.Name)
	}
	return coordinator.VerifyQuiescent(ctx, state, names)
}

func (coordinator deckyRuntimeCoordinator) KeepDisabled(ctx context.Context, state *Quiescence, name string) error {
	if state == nil {
		return errors.New("Decky quiescence state is missing")
	}
	values := append(append([]string(nil), state.TemporaryDisabled...), name)
	canonical, err := canonicalPluginNames(values)
	if err != nil {
		return err
	}
	if err := coordinator.controller.SetDisabledPlugins(ctx, canonical); err != nil {
		return fmt.Errorf("persist disabled state before plugin installation: %w", err)
	}
	state.TemporaryDisabled = canonical
	return nil
}

func (coordinator deckyRuntimeCoordinator) VerifyQuiescent(ctx context.Context, state Quiescence, expectedNames []string) error {
	_, err := coordinator.verifiedDisabledState(ctx, expectedNames)
	return err
}

// SynchronizeQuiescence records Decky's verified temporary disabled state
// after Decky-owned install/uninstall operations. Decky may remove an
// uninstalled identity itself, so stale pre-operation bookkeeping is never
// treated as an invariant.
func (coordinator deckyRuntimeCoordinator) SynchronizeQuiescence(ctx context.Context, state *Quiescence, expectedNames []string) error {
	if state == nil {
		return errors.New("Decky quiescence state is missing")
	}
	disabled, err := coordinator.verifiedDisabledState(ctx, expectedNames)
	if err != nil {
		return err
	}
	state.TemporaryDisabled = disabled
	return nil
}

// RestoreOriginal is the only bounded recovery path for the temporary
// disabled-plugin state. It proves both Decky's persisted list and every live
// plugin's effective state match the state captured before quiescence.
func (coordinator deckyRuntimeCoordinator) RestoreOriginal(ctx context.Context, target TargetReference, state Quiescence) error {
	setErr := coordinator.controller.SetDisabledPlugins(ctx, state.OriginalDisabled)
	restartErr := coordinator.restarter.Restart(ctx, target.Decky)
	verifyErr := coordinator.verifyOriginalState(ctx, state)
	return errors.Join(setErr, restartErr, verifyErr)
}

func (coordinator deckyRuntimeCoordinator) verifyOriginalState(ctx context.Context, state Quiescence) error {
	expectedNames := quiescencePluginNames(state.OriginalInventory)
	expectedNames, err := canonicalPluginNames(expectedNames)
	if err != nil {
		return err
	}
	disabled, err := canonicalPluginNames(state.OriginalDisabled)
	if err != nil {
		return fmt.Errorf("validate original Decky disabled-plugin state: %w", err)
	}
	inventory, err := coordinator.controller.Inventory(ctx)
	if err != nil {
		return fmt.Errorf("re-read Decky plugin inventory after cleanup: %w", err)
	}
	actualNames := quiescencePluginNames(inventory)
	if !equalPluginNames(actualNames, expectedNames) {
		return errors.New("Decky plugin inventory changed during temporary-state cleanup")
	}
	actualDisabled, err := coordinator.controller.DisabledPlugins(ctx)
	if err != nil {
		return fmt.Errorf("re-read Decky disabled-plugin state after cleanup: %w", err)
	}
	if !equalPluginNames(actualDisabled, disabled) {
		return errors.New("Decky disabled-plugin state was not restored exactly")
	}
	for _, plugin := range inventory {
		if plugin.Disabled != containsPluginName(disabled, plugin.Name) {
			return fmt.Errorf("Decky plugin %q effective disabled state was not restored", plugin.Name)
		}
	}
	return nil
}

func (coordinator deckyRuntimeCoordinator) verifiedDisabledState(ctx context.Context, expectedNames []string) ([]string, error) {
	expected, err := canonicalPluginNames(expectedNames)
	if err != nil {
		return nil, err
	}
	inventory, err := coordinator.controller.Inventory(ctx)
	if err != nil {
		return nil, fmt.Errorf("re-read Decky plugin inventory: %w", err)
	}
	actual := make([]string, 0, len(inventory))
	for _, plugin := range inventory {
		if !plugin.Disabled {
			return nil, fmt.Errorf("Decky plugin %q is still active", plugin.Name)
		}
		actual = append(actual, plugin.Name)
	}
	if !equalPluginNames(actual, expected) {
		return nil, errors.New("Decky plugin inventory changed unexpectedly during restore")
	}
	disabled, err := coordinator.controller.DisabledPlugins(ctx)
	if err != nil {
		return nil, fmt.Errorf("re-read Decky disabled-plugin state: %w", err)
	}
	disabled, err = canonicalPluginNames(disabled)
	if err != nil {
		return nil, fmt.Errorf("validate Decky disabled-plugin state: %w", err)
	}
	for _, name := range actual {
		if !containsPluginName(disabled, name) {
			return nil, fmt.Errorf("Decky plugin %q is missing from temporary disabled-plugin state", name)
		}
	}
	return disabled, nil
}

func canonicalPluginNames(values []string) ([]string, error) {
	if len(values) > 512 {
		return nil, errors.New("Decky plugin inventory exceeds the limit")
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !safePluginIdentity(value) {
			return nil, errors.New("Decky plugin identity is unsafe")
		}
		if _, duplicate := seen[value]; !duplicate {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result, nil
}

func equalPluginNames(first, second []string) bool {
	first, firstErr := canonicalPluginNames(first)
	second, secondErr := canonicalPluginNames(second)
	if firstErr != nil || secondErr != nil || len(first) != len(second) {
		return false
	}
	for i := range first {
		if first[i] != second[i] {
			return false
		}
	}
	return true
}

func containsPluginName(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func quiescencePluginNames(values []deckyapi.PluginStatus) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.Name)
	}
	return result
}

func removePluginName(values []string, wanted string) []string {
	result := values[:0]
	for _, value := range values {
		if value != wanted {
			result = append(result, value)
		}
	}
	return result
}

func removePluginStatus(values []deckyapi.PluginStatus, wanted string) []deckyapi.PluginStatus {
	result := values[:0]
	for _, value := range values {
		if value.Name != wanted {
			result = append(result, value)
		}
	}
	return result
}

func replacePluginStatus(values []deckyapi.PluginStatus, oldName, newName, version string) []deckyapi.PluginStatus {
	values = removePluginStatus(values, oldName)
	return append(values, deckyapi.PluginStatus{Name: newName, Version: version, Disabled: true})
}
