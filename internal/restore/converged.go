package restore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

// verifyConverged proves that the supported live inventory now matches the
// selected snapshot. It is intentionally separate from stale-plan validation:
// a successful convergence must no longer match the pre-restore fingerprint.
func verifyConverged(ctx context.Context, plan Plan, resourceLimits limits.Limits, installer deckyapi.Installer) error {
	value, err := snapshot.ValidateContext(ctx, plan.Snapshot.Path, resourceLimits)
	if err != nil {
		return fmt.Errorf("validate selected snapshot: %w", err)
	}
	if value.SnapshotID != plan.Snapshot.SnapshotID || value.CreatedUTC != plan.Snapshot.CreatedUTC {
		return errors.New("selected snapshot identity changed during restore")
	}
	paths := platform.Paths{Home: plan.Target.Home, Decky: plan.Target.Decky, Steam: plan.Target.Steam, State: plan.Target.State}
	current := make([]Action, 0, len(value.Files))
	for _, entry := range value.Files {
		action, mapped, err := buildAction(paths, entry, resourceLimits)
		if err != nil {
			return err
		}
		if mapped {
			current = append(current, action)
		}
	}
	current, preserved := splitIncompatibleSettings(paths, plan.Snapshot.SnapshotID, plan.Plugins, current)
	for _, action := range current {
		if action.Operation != "unchanged" {
			return fmt.Errorf("snapshot target is not converged: %s", action.LogicalPath)
		}
	}
	for _, setting := range preserved {
		if setting.Operation == "blocked" {
			return fmt.Errorf("incompatible setting cannot be verified: %s", setting.LogicalPath)
		}
		action := Action{LogicalPath: setting.LogicalPath, TargetRoot: setting.PreserveRoot, TargetPath: setting.PreservePath, Operation: "create", Size: setting.Size, SHA256: setting.SHA256, DesiredMode: 0o600}
		if err := verifyAppliedTarget(action); err != nil {
			return fmt.Errorf("preserved incompatible setting is unavailable: %s", setting.LogicalPath)
		}
	}
	removals, err := buildConvergenceActions(ctx, paths, value, plan.AppVersion, planCreationTime(plan), resourceLimits)
	if err != nil {
		return err
	}
	if len(removals) != 0 {
		return fmt.Errorf("supported live state added after the snapshot remains: %s", removals[0].LogicalPath)
	}
	plugins, err := buildPluginActions(ctx, paths, plan.Snapshot.SnapshotID, plan.Plugins, resourceLimits, installer)
	if err != nil {
		return err
	}
	for _, action := range plugins {
		if action.Operation != "unchanged" {
			return fmt.Errorf("Decky plugin inventory is not converged: %s", action.Directory)
		}
	}
	return nil
}

func planCreationTime(plan Plan) time.Time {
	created, err := time.Parse(time.RFC3339Nano, plan.CreatedUTC)
	if err != nil {
		return time.Unix(0, 0).UTC()
	}
	return created
}
