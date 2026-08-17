package restore

import (
	"context"
	"errors"
	"fmt"

	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

type PendingRestoreState string

const (
	PendingRestoreNone          PendingRestoreState = "none"
	PendingRestoreRestartNeeded PendingRestoreState = "restart_required"
	PendingRestoreComplete      PendingRestoreState = "complete"
	PendingRestoreAttention     PendingRestoreState = "attention"
)

// CheckPendingRestore is the bounded foreground continuation run on normal
// application launch. It is entirely read-only until all checks pass and the
// transaction marker can be retired atomically.
func CheckPendingRestore(ctx context.Context, paths platform.Paths, appVersion string, resolver pluginstore.Resolver, controller deckyapi.Controller, rebooter Rebooter) (PendingRestoreState, error) {
	marker, pending, err := loadIncompleteTransaction(paths.Home, paths.State)
	if err != nil || !pending {
		return PendingRestoreNone, err
	}
	if marker.Phase != "awaiting_reboot" && marker.Phase != "awaiting_post_boot_verification" {
		return PendingRestoreAttention, errors.New("an incomplete restore requires recovery")
	}
	if rebooter == nil {
		return PendingRestoreAttention, errors.New("pending restore reboot boundary is unavailable")
	}
	bootID, err := rebooter.BootID(ctx)
	if err != nil {
		return PendingRestoreAttention, err
	}
	if bootID == marker.PreRebootBootID {
		return PendingRestoreRestartNeeded, nil
	}
	if controller == nil {
		return PendingRestoreAttention, errors.New("pending restore verification boundary is unavailable")
	}
	if marker.Phase == "awaiting_reboot" {
		marker.Phase = "awaiting_post_boot_verification"
		if err := updateIncompleteTransaction(paths.Home, paths.State, marker); err != nil {
			return PendingRestoreAttention, err
		}
	}
	if _, err := snapshot.ValidateContext(ctx, marker.SnapshotPath, limits.Default()); err != nil {
		return PendingRestoreAttention, fmt.Errorf("validate pending restore snapshot: %w", err)
	}
	recovery, err := snapshot.ValidateContext(ctx, marker.RecoveryPath, limits.Default())
	if err != nil || recovery.SnapshotID == "" {
		return PendingRestoreAttention, errors.New("validate pending restore recovery snapshot")
	}
	if err := controller.Probe(ctx, paths.Decky); err != nil {
		return PendingRestoreAttention, fmt.Errorf("verify Decky after reboot: %w", err)
	}
	if resolver == nil {
		return PendingRestoreAttention, errors.New("pending restore plugin resolver is unavailable")
	}
	plan, err := BuildPlan(ctx, PlanOptions{Paths: paths, SnapshotPath: marker.SnapshotPath, AppVersion: appVersion, Limits: limits.Default(), Resolver: resolver, DeckyInstaller: controller})
	if err != nil {
		return PendingRestoreAttention, fmt.Errorf("re-plan restored state: %w", err)
	}
	if plan.Snapshot.SnapshotID != marker.SnapshotID || plan.HasMutations() || plan.Blocking {
		return PendingRestoreAttention, errors.New("restored customization does not match the selected backup")
	}
	if err := removeIncompleteTransaction(paths.Home, paths.State); err != nil {
		return PendingRestoreAttention, err
	}
	return PendingRestoreComplete, nil
}
