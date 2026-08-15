package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

// Revalidate confirms that both the immutable snapshot and every target still
// match the state approved in the plan. It performs no production writes.
func Revalidate(plan Plan, resourceLimits limits.Limits, installers ...deckyapi.Installer) error {
	var installer deckyapi.Installer
	if len(installers) > 0 {
		installer = installers[0]
	}
	return revalidate(context.Background(), plan, resourceLimits, installer)
}

func revalidate(ctx context.Context, plan Plan, resourceLimits limits.Limits, installer deckyapi.Installer) error {
	if err := ValidatePlan(plan); err != nil {
		return err
	}
	if err := resourceLimits.Validate(); err != nil {
		return err
	}
	value, err := snapshot.Validate(plan.Snapshot.Path, resourceLimits)
	if err != nil {
		return fmt.Errorf("revalidate restore snapshot: %w", err)
	}
	if value.SnapshotID != plan.Snapshot.SnapshotID || value.CreatedUTC != plan.Snapshot.CreatedUTC {
		return errors.New("restore snapshot identity no longer matches the approved plan")
	}
	hash, info, err := hashRegularFile(plan.Snapshot.Path, resourceLimits.MaxTotalSize+resourceLimits.MaxManifestSize)
	if err != nil || hash != plan.Snapshot.SHA256 || info.Size() != plan.Snapshot.Size {
		return errors.New("restore snapshot fingerprint no longer matches the approved plan")
	}

	paths := platform.Paths{Home: plan.Target.Home, Decky: plan.Target.Decky, Steam: plan.Target.Steam, State: plan.Target.State}
	current := make([]Action, 0, len(value.Files))
	for _, entry := range value.Files {
		action, mapped, err := buildAction(paths, entry, resourceLimits)
		if err != nil {
			return fmt.Errorf("rebuild target fingerprint for %q: %w", entry.LogicalPath, err)
		}
		if !mapped {
			continue
		}
		current = append(current, action)
	}
	currentPlugins, err := buildPluginActions(ctx, paths, plan.Snapshot.SnapshotID, plan.Plugins, resourceLimits, installer)
	if err != nil {
		return err
	}
	currentPlan := Plan{Actions: current, PluginActions: currentPlugins}
	bindDeckyAPISettingsRecovery(&currentPlan, paths, resourceLimits)
	current = currentPlan.Actions
	currentPlugins = currentPlan.PluginActions
	current, currentPreserved := splitIncompatibleSettings(paths, plan.Snapshot.SnapshotID, plan.Plugins, current)
	if fingerprintTargets(current, currentPlugins, currentPreserved, currentPlan.DeckyLoaderGuard) != plan.TargetFingerprint {
		return errors.New("restore targets changed after the plan was approved")
	}
	return nil
}

func verifyAppliedTarget(action Action) error {
	info, err := os.Lstat(action.TargetPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || isLinkOrReparsePoint(info) || info.Size() != action.Size {
		return errors.New("applied target is not the expected regular file")
	}
	if err := platformOwnedByCurrentUser(info); err != nil {
		return err
	}
	if !appliedModeMatches(uint32(info.Mode().Perm()), action.DesiredMode) {
		return errors.New("applied target permissions do not match the plan")
	}
	hash, _, err := hashRegularFile(action.TargetPath, action.Size)
	if err != nil || hash != action.SHA256 {
		return errors.New("applied target checksum does not match the snapshot")
	}
	return nil
}

func relativeToHome(home, target string) (string, error) {
	relative, err := filepath.Rel(home, target)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) {
		return "", errors.New("target is not safely relative to the target home")
	}
	if len(relative) >= 3 && relative[:3] == ".."+string(filepath.Separator) {
		return "", errors.New("target escapes the target home")
	}
	return relative, nil
}
