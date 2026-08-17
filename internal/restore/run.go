package restore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

type ActionResult struct {
	LogicalPath string `json:"logical_path"`
	Component   string `json:"component"`
	Operation   string `json:"operation"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
}

type Report struct {
	PlanID               string         `json:"plan_id"`
	StartedUTC           string         `json:"started_utc"`
	CompletedUTC         string         `json:"completed_utc"`
	Status               string         `json:"status"`
	SnapshotPath         string         `json:"snapshot_path"`
	RecoverySnapshotPath string         `json:"recovery_snapshot_path"`
	ReportPath           string         `json:"report_path,omitempty"`
	Actions              []ActionResult `json:"actions"`
	Error                string         `json:"error,omitempty"`
	RollbackError        string         `json:"rollback_error,omitempty"`
}

type RunOptions struct {
	Plan               Plan
	ApprovedPlanID     string
	ApprovedHash       string
	Limits             limits.Limits
	WorkDirectory      string
	RecoveryDirectory  string
	ReportDirectory    string
	Now                func() time.Time
	AvailableBytes     func(string) (uint64, error)
	BeforeAction       func(Action) error
	BeforePlugin       func(PluginAction) error
	HTTPClient         *http.Client
	DeckyInstaller     deckyapi.Installer
	RuntimeCoordinator RuntimeCoordinator
	Rebooter           Rebooter
}

// RuntimeCoordinator refreshes the bounded Decky/Steam runtime after an
// otherwise successful mutation. Restore deliberately treats an unavailable
// coordinator or a failed refresh as a transaction failure: written files are
// not reported as a completed restore while the live UI can still be stale.
type RuntimeCoordinator interface {
	Refresh(context.Context, TargetReference) error
}

type deckyRuntimeCoordinator struct {
	controller deckyapi.Controller
	restarter  deckyapi.Restarter
}

// NewDeckyRuntimeCoordinator exposes only Decky's supported, version-bounded
// restart route. It never executes a shell command or controls arbitrary
// processes.
func NewDeckyRuntimeCoordinator(controller deckyapi.Controller) RuntimeCoordinator {
	if controller == nil {
		return nil
	}
	return deckyRuntimeCoordinator{controller: controller, restarter: controller}
}

func (coordinator deckyRuntimeCoordinator) Refresh(ctx context.Context, target TargetReference) error {
	if err := coordinator.restarter.Restart(ctx, target.Decky); err != nil {
		return fmt.Errorf("refresh Decky Loader and Steam runtime: %w", err)
	}
	return nil
}

func (coordinator deckyRuntimeCoordinator) QuiesceSteam(ctx context.Context) error {
	return gracefulSteamShutdown(ctx)
}

func requiresRuntimeRefresh(plan Plan) bool {
	for _, action := range plan.Actions {
		if action.Operation != "create" && action.Operation != "replace" && action.Operation != "remove" {
			continue
		}
		switch action.Component {
		case "decky", "css-loader", "steam":
			return true
		}
	}
	for _, action := range plan.PluginActions {
		if action.Operation == "create" || action.Operation == "replace" || action.Operation == "remove" {
			return true
		}
	}
	return false
}

// Run applies an exact approved plan only after stale-state checks, selected
// payload staging and validation of a recovery snapshot. On any action failure,
// all already-applied actions are rolled back before the function returns.
func Run(ctx context.Context, options RunOptions) (Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	started := options.Now().UTC()
	report := Report{PlanID: options.Plan.PlanID, StartedUTC: started.Format(time.RFC3339Nano), SnapshotPath: options.Plan.Snapshot.Path, Status: "failed_before_mutation"}
	finish := func() { report.CompletedUTC = options.Now().UTC().Format(time.RFC3339Nano) }
	if options.ApprovedPlanID != options.Plan.PlanID || options.ApprovedHash != options.Plan.ApprovalHash {
		finish()
		return report, errors.New("exact plan ID and approval hash are required")
	}
	if options.Plan.Blocking {
		finish()
		return report, errors.New("restore plan contains blocking actions or plugin resolutions")
	}
	if err := validatePlatformRestoreSupport(); err != nil {
		finish()
		return report, err
	}
	if err := revalidate(ctx, options.Plan, options.Limits, options.DeckyInstaller); err != nil {
		finish()
		return report, err
	}
	if !options.Plan.HasMutations() {
		report.Status = "already_matches"
		finish()
		return report, nil
	}
	for _, directory := range []string{options.WorkDirectory, options.RecoveryDirectory, options.ReportDirectory} {
		if !filepath.IsAbs(directory) {
			finish()
			return report, errors.New("restore work, recovery and report directories must be absolute")
		}
		if _, err := relativeToHome(options.Plan.Target.State, filepath.Join(directory, ".deck-snapshot-scope")); err != nil {
			finish()
			return report, errors.New("restore work, recovery and report directories must be beneath the application state root")
		}
		if err := ValidateTarget(options.Plan.Target.State, filepath.Join(directory, ".deck-snapshot-scope")); err != nil {
			finish()
			return report, fmt.Errorf("restore state directory is unsafe: %w", err)
		}
	}
	for _, directory := range []string{options.WorkDirectory, options.RecoveryDirectory, options.ReportDirectory} {
		if err := ensureSecureDirectory(options.Plan.Target.Home, directory); err != nil {
			finish()
			return report, fmt.Errorf("prepare confined restore state directory: %w", err)
		}
	}
	if marker, pending, err := loadIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State); err != nil {
		finish()
		return report, fmt.Errorf("inspect incomplete restore transaction: %w", err)
	} else if pending {
		finish()
		return report, fmt.Errorf("restore recovery is required for incomplete transaction %q (recovery snapshot: %s)", marker.PlanID, marker.RecoveryPath)
	}
	available := options.AvailableBytes
	if available == nil {
		available = AvailableBytes
	}
	spaceRoots := []string{options.Plan.Target.Home, options.Plan.Target.State, options.Plan.Target.Decky, options.Plan.Target.Steam}
	checkedRoots := make(map[string]struct{}, len(spaceRoots))
	for _, root := range spaceRoots {
		probe, err := nearestExistingDirectory(root)
		if err != nil {
			finish()
			return report, fmt.Errorf("resolve restore filesystem for %q: %w", root, err)
		}
		if _, checked := checkedRoots[probe]; checked {
			continue
		}
		checkedRoots[probe] = struct{}{}
		free, err := available(probe)
		if err != nil {
			finish()
			return report, fmt.Errorf("check restore free space for %q: %w", root, err)
		}
		if uint64(options.Plan.RequiredFreeBytes) > free {
			finish()
			return report, fmt.Errorf("restore requires %d bytes on each involved filesystem but %q has only %d bytes available", options.Plan.RequiredFreeBytes, root, free)
		}
	}
	privateRun, err := createPrivateDirectory(options.Plan.Target.Home, options.WorkDirectory, ".restore-run-")
	if err != nil {
		finish()
		return report, err
	}
	defer privateRun.Close()
	runDirectory := privateRun.Path
	stagingDirectory := filepath.Join(runDirectory, "payload")
	if err := privateRun.root.Mkdir("payload", 0o700); err != nil {
		finish()
		return report, err
	}
	selected := make([]string, 0, len(options.Plan.Actions))
	for _, action := range options.Plan.Actions {
		if action.Operation == "create" || action.Operation == "replace" {
			selected = append(selected, action.LogicalPath)
		}
	}
	for _, action := range options.Plan.PreservedSettings {
		if action.Operation == "create" {
			selected = append(selected, action.LogicalPath)
		}
	}
	staged, stagedManifest, err := snapshot.StageSelected(ctx, options.Plan.Snapshot.Path, stagingDirectory, selected, options.Limits)
	if err != nil {
		cleanupStage(runDirectory, stagingDirectory, staged)
		finish()
		return report, fmt.Errorf("stage restore payloads: %w", err)
	}
	if stagedManifest.SnapshotID != options.Plan.Snapshot.SnapshotID {
		cleanupStage(runDirectory, stagingDirectory, staged)
		finish()
		return report, errors.New("staged snapshot identity does not match the plan")
	}
	preparedPlugins := make(map[string]pluginstore.PreparedPackage)
	var pluginWorkspace string
	filesystemPlugins := 0
	deckyAPIPlugins := 0
	for _, action := range options.Plan.PluginActions {
		if action.Operation == "create" || action.Operation == "replace" || action.Operation == "remove" {
			switch action.Method {
			case pluginMethodFilesystem:
				filesystemPlugins++
			case pluginMethodDeckyAPI:
				deckyAPIPlugins++
			}
		}
	}
	if filesystemPlugins > 0 {
		const pluginWorkspaceName = "filesystem-plugin-packages"
		if err := privateRun.root.Mkdir(pluginWorkspaceName, 0o700); err != nil {
			finish()
			return report, err
		}
		pluginWorkspace = filepath.Join(runDirectory, pluginWorkspaceName)
	}
	if deckyAPIPlugins > 0 {
		if err := privateRun.root.Mkdir("decky-api-packages", 0o700); err != nil {
			finish()
			return report, err
		}
	}
	if filesystemPlugins+deckyAPIPlugins > 0 {
		for index, action := range options.Plan.PluginActions {
			if action.Operation != "create" && action.Operation != "replace" {
				continue
			}
			resolution, exists := resolutionForDirectory(options.Plan.Plugins, action.Directory)
			if !exists {
				finish()
				return report, fmt.Errorf("plugin resolution is missing for %q", action.Directory)
			}
			workspaceName := fmt.Sprintf("package-%04d", index)
			var workspace string
			if action.Method == pluginMethodFilesystem {
				workspace = filepath.Join(pluginWorkspace, workspaceName)
				if err := privateRun.root.Mkdir(filepath.Join("filesystem-plugin-packages", workspaceName), 0o700); err != nil {
					finish()
					return report, err
				}
			} else {
				workspace = filepath.Join(runDirectory, "decky-api-packages", workspaceName)
				if err := privateRun.root.Mkdir(filepath.Join("decky-api-packages", workspaceName), 0o700); err != nil {
					finish()
					return report, err
				}
			}
			prepared, err := pluginstore.PreparePackage(ctx, resolution, workspace, options.HTTPClient)
			if err != nil {
				finish()
				return report, fmt.Errorf("prepare plugin %q: %w", action.Directory, err)
			}
			preparedPlugins[action.Directory] = prepared
		}
	}
	if err := revalidate(ctx, options.Plan, options.Limits, options.DeckyInstaller); err != nil {
		cleanupStage(runDirectory, stagingDirectory, staged)
		finish()
		return report, err
	}
	needsRuntimeRefresh := requiresRuntimeRefresh(options.Plan)
	preRebootBootID := ""
	if needsRuntimeRefresh {
		if options.Rebooter == nil {
			finish()
			return report, errors.New("restore cannot safely restart this Steam Deck afterward")
		}
		if err := options.Rebooter.Preflight(ctx); err != nil {
			finish()
			return report, fmt.Errorf("restore cannot safely restart this Steam Deck afterward: %w", err)
		}
		preRebootBootID, err = options.Rebooter.BootID(ctx)
		if err != nil {
			finish()
			return report, fmt.Errorf("record pre-restore boot identity: %w", err)
		}
	}

	recoveryWorkspace := filepath.Join(runDirectory, "recovery-output")
	if err := privateRun.root.Mkdir("recovery-output", 0o700); err != nil {
		cleanupStage(runDirectory, stagingDirectory, staged)
		finish()
		return report, err
	}
	recovery, err := createRecovery(ctx, options.Plan, recoveryWorkspace, started, options.Limits)
	if err != nil {
		cleanupStage(runDirectory, stagingDirectory, staged)
		finish()
		return report, err
	}
	publishedRecoveryPath := filepath.Join(options.RecoveryDirectory, filepath.Base(recovery.Path))
	if err := moveNoReplace(options.Plan.Target.Home, recovery.Path, publishedRecoveryPath, false); err != nil {
		cleanupStage(runDirectory, stagingDirectory, staged)
		finish()
		return report, fmt.Errorf("publish validated recovery snapshot without replacement: %w", err)
	}
	recovery.Path = publishedRecoveryPath
	if validated, err := snapshot.ValidateContext(ctx, recovery.Path, options.Limits); err != nil || !recoveryManifestsEqual(recovery.Manifest, validated) {
		cleanupStage(runDirectory, stagingDirectory, staged)
		finish()
		return report, errors.Join(errors.New("published recovery snapshot failed final validation"), err)
	}
	report.RecoverySnapshotPath = recovery.Path
	recoverySelected := make([]string, 0)
	for _, action := range options.Plan.Actions {
		if action.Operation == "replace" || action.Operation == "remove" {
			recoverySelected = append(recoverySelected, action.LogicalPath)
		}
	}
	if guard := options.Plan.DeckyLoaderGuard; guard != nil && guard.Operation == "restore" {
		recoverySelected = append(recoverySelected, deckyLoaderRecoveryLogicalPath)
	}
	for _, action := range options.Plan.PluginActions {
		if (action.Operation != "replace" && action.Operation != "remove") || action.Method != pluginMethodDeckyAPI {
			continue
		}
		prefix := "recovery/plugins/" + action.Directory + "/"
		for _, entry := range recovery.Manifest.Files {
			if strings.HasPrefix(entry.LogicalPath, prefix) {
				recoverySelected = append(recoverySelected, entry.LogicalPath)
			}
		}
	}
	recoveryStaged := map[string]snapshot.StagedFile{}
	recoveryStageDirectory := filepath.Join(runDirectory, "recovery")
	if len(recoverySelected) > 0 {
		if err := privateRun.root.Mkdir("recovery", 0o700); err != nil {
			cleanupStage(runDirectory, stagingDirectory, staged)
			finish()
			return report, err
		}
		var stagedRecoveryManifest manifest.Manifest
		recoveryStaged, stagedRecoveryManifest, err = snapshot.StageSelected(ctx, recovery.Path, recoveryStageDirectory, recoverySelected, options.Limits)
		if err != nil {
			cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
			cleanupStage(runDirectory, stagingDirectory, staged)
			finish()
			return report, fmt.Errorf("stage validated recovery payloads: %w", err)
		}
		if err := validateStagedRecovery(options.Plan, recovery.Manifest, stagedRecoveryManifest, recoveryStaged); err != nil {
			cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
			cleanupStage(runDirectory, stagingDirectory, staged)
			finish()
			return report, err
		}
	}
	recoveryPluginPackages := map[string]recoveryPluginPackage{}
	if deckyAPIPlugins > 0 {
		recoveryPluginPackages, err = createRecoveryPluginPackages(ctx, filepath.Join(runDirectory, "recovery-plugin-packages"), options.Plan, recovery.Manifest, recoveryStaged, options.Limits)
		if err != nil {
			cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
			cleanupStage(runDirectory, stagingDirectory, staged)
			finish()
			return report, err
		}
	}
	if err := revalidate(ctx, options.Plan, options.Limits, options.DeckyInstaller); err != nil {
		cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
		cleanupStage(runDirectory, stagingDirectory, staged)
		finish()
		return report, err
	}
	if needsRuntimeRefresh && options.RuntimeCoordinator == nil {
		err := errors.New("restore requires a verified Decky runtime refresh coordinator")
		report.Error = err.Error()
		finish()
		_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
		cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
		cleanupStage(runDirectory, stagingDirectory, staged)
		return report, err
	}
	var quiescence Quiescence
	var quiescer QuiescenceCoordinator
	var transactionMarker incompleteTransactionMarker
	if needsRuntimeRefresh {
		var supported bool
		quiescer, supported = options.RuntimeCoordinator.(QuiescenceCoordinator)
		if !supported {
			err := errors.New("restore requires the supported Decky quiescence coordinator")
			report.Error = err.Error()
			finish()
			_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
			cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
			cleanupStage(runDirectory, stagingDirectory, staged)
			return report, err
		}
		quiescence, err = quiescer.PlanQuiescence(ctx, options.Plan.Target)
		if err != nil {
			finish()
			cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
			cleanupStage(runDirectory, stagingDirectory, staged)
			return report, err
		}
		transactionMarker = incompleteTransactionMarker{Schema: 2, PlanID: options.Plan.PlanID, SnapshotID: options.Plan.Snapshot.SnapshotID, SnapshotPath: options.Plan.Snapshot.Path, RecoveryPath: recovery.Path, OriginalPluginInventory: quiescencePluginNames(quiescence.OriginalInventory), OriginalDisabledPlugins: append([]string(nil), quiescence.OriginalDisabled...), TemporaryDisabledPlugins: append([]string(nil), quiescence.TemporaryDisabled...), PreRebootBootID: preRebootBootID, Phase: "prepared"}
		if err := saveIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); err != nil {
			finish()
			cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
			cleanupStage(runDirectory, stagingDirectory, staged)
			return report, fmt.Errorf("record incomplete restore transaction: %w", err)
		}
		if err := quiescer.Quiesce(ctx, options.Plan.Target, quiescence); err != nil {
			report.Error = err.Error()
			cleanupErr := quiescer.RestoreOriginal(context.Background(), options.Plan.Target, quiescence)
			if cleanupErr != nil {
				report.Status = "rollback_failed"
				report.RollbackError = cleanupErr.Error()
				transactionMarker.Phase = "recovery_required"
				if markerErr := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); markerErr != nil {
					report.RollbackError = errors.Join(cleanupErr, fmt.Errorf("record required Decky recovery: %w", markerErr)).Error()
				}
			} else {
				report.Status = "rolled_back"
				if markerErr := removeIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State); markerErr != nil {
					report.Status = "rollback_failed"
					report.RollbackError = markerErr.Error()
				}
			}
			finish()
			_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
			cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
			cleanupStage(runDirectory, stagingDirectory, staged)
			return report, err
		}
		rollbackQuiescenceOnly := func(failure error) (Report, error) {
			report.Error = failure.Error()
			cleanupErr := quiescer.RestoreOriginal(context.Background(), options.Plan.Target, quiescence)
			if cleanupErr != nil {
				report.Status = "rollback_failed"
				report.RollbackError = cleanupErr.Error()
				transactionMarker.Phase = "recovery_required"
				if markerErr := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); markerErr != nil {
					report.RollbackError = errors.Join(cleanupErr, fmt.Errorf("record required Decky recovery: %w", markerErr)).Error()
				}
			} else {
				report.Status = "rolled_back"
				if markerErr := removeIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State); markerErr != nil {
					report.Status = "rollback_failed"
					report.RollbackError = markerErr.Error()
				}
			}
			finish()
			_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
			cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
			cleanupStage(runDirectory, stagingDirectory, staged)
			return report, failure
		}
		transactionMarker.Phase = "plugins_quiesced"
		if err := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); err != nil {
			return rollbackQuiescenceOnly(fmt.Errorf("record Decky quiescence: %w", err))
		}
		transactionMarker.Phase = "plugin_convergence"
		if err := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); err != nil {
			return rollbackQuiescenceOnly(fmt.Errorf("record plugin convergence: %w", err))
		}
	}

	installedPlugins := make([]installedPlugin, 0, len(options.Plan.PluginActions))
	rollbackPlugins := func() error {
		rollbackErr := rollbackInstalledPluginsWithDecky(context.Background(), options.Plan.Target.Home, installedPlugins, options.Limits, options.DeckyInstaller, recoveryPluginPackages)
		apiMutation := false
		for _, installed := range installedPlugins {
			apiMutation = apiMutation || (installed.Action.Method == pluginMethodDeckyAPI && installed.MutationStarted)
		}
		if apiMutation {
			rollbackErr = errors.Join(rollbackErr, restoreDeckySettingsSideEffects(context.Background(), options.Plan.Target.Home, options.Plan.DeckyLoaderGuard, recoveryStaged, options.Limits))
		}
		if needsRuntimeRefresh {
			rollbackErr = errors.Join(rollbackErr, quiescer.RestoreOriginal(context.Background(), options.Plan.Target, quiescence))
		}
		return rollbackErr
	}
	failPluginQuiescence := func(failure error) (Report, error) {
		report.Error = failure.Error()
		rollbackErr := rollbackPlugins()
		if revalidateErr := revalidate(context.Background(), options.Plan, options.Limits, options.DeckyInstaller); revalidateErr != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("verify rolled-back target state: %w", revalidateErr))
		}
		if rollbackErr != nil {
			report.Status = "rollback_failed"
			report.RollbackError = rollbackErr.Error()
			transactionMarker.Phase = "recovery_required"
			if markerErr := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); markerErr != nil {
				report.RollbackError = errors.Join(rollbackErr, fmt.Errorf("record required Decky recovery: %w", markerErr)).Error()
			}
		} else {
			report.Status = "rolled_back"
			if markerErr := removeIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State); markerErr != nil {
				report.Status = "rollback_failed"
				report.RollbackError = markerErr.Error()
			}
		}
		finish()
		_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
		cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
		cleanupStage(runDirectory, stagingDirectory, staged)
		return report, failure
	}
	for _, action := range options.Plan.PluginActions {
		result := ActionResult{LogicalPath: "plugin/" + action.Directory, Component: "decky/plugins", Operation: action.Operation}
		if action.Operation == "unchanged" {
			result.Status = "unchanged"
			report.Actions = append(report.Actions, result)
			continue
		}
		if err := ctx.Err(); err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			report.Actions = append(report.Actions, result)
			return failPluginQuiescence(err)
		}
		if options.BeforePlugin != nil {
			if err := options.BeforePlugin(action); err != nil {
				result.Status = "failed"
				result.Message = err.Error()
				report.Actions = append(report.Actions, result)
				return failPluginQuiescence(err)
			}
		}
		if action.Operation == "remove" {
			installed, removeErr := removeDeckyPlugin(ctx, options.DeckyInstaller, action, options.Limits)
			if removeErr != nil {
				if installed.MutationStarted {
					installedPlugins = append(installedPlugins, installed)
				}
				result.Status = "failed"
				result.Message = removeErr.Error()
				report.Actions = append(report.Actions, result)
				return failPluginQuiescence(removeErr)
			}
			installedPlugins = append(installedPlugins, installed)
			if needsRuntimeRefresh {
				expected := quiescencePluginNames(quiescence.Inventory)
				expected = removePluginName(expected, action.ExistingName)
				if err := quiescer.SynchronizeQuiescence(ctx, &quiescence, expected); err != nil {
					return failPluginQuiescence(err)
				}
				quiescence.Inventory = removePluginStatus(quiescence.Inventory, action.ExistingName)
				transactionMarker.TemporaryDisabledPlugins = append([]string(nil), quiescence.TemporaryDisabled...)
				if err := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); err != nil {
					return failPluginQuiescence(fmt.Errorf("record synchronized Decky quiescence: %w", err))
				}
			}
			result.Status = "applied"
			result.Message = "Extra plugin removed through Decky Loader and retained in the validated recovery snapshot."
			report.Actions = append(report.Actions, result)
			continue
		}
		prepared, exists := preparedPlugins[action.Directory]
		if !exists {
			err := fmt.Errorf("prepared plugin is missing for %q", action.Directory)
			result.Status = "failed"
			result.Message = err.Error()
			report.Actions = append(report.Actions, result)
			return failPluginQuiescence(err)
		}
		resolution, resolved := resolutionForDirectory(options.Plan.Plugins, action.Directory)
		if !resolved {
			err := fmt.Errorf("approved plugin resolution is missing for %q", action.Directory)
			result.Status = "failed"
			result.Message = err.Error()
			report.Actions = append(report.Actions, result)
			return failPluginQuiescence(err)
		}
		if needsRuntimeRefresh && action.Method == pluginMethodDeckyAPI {
			if err := quiescer.KeepDisabled(ctx, &quiescence, resolution.StoreName); err != nil {
				return failPluginQuiescence(err)
			}
		}
		var installed installedPlugin
		if action.Method == pluginMethodDeckyAPI {
			installed, err = installPreparedPluginWithDecky(ctx, options.DeckyInstaller, action, prepared, resolution, options.Limits)
		} else {
			installed, err = installPreparedPlugin(options.Plan.Target.Home, action, prepared, options.Limits)
		}
		if err != nil {
			var incomplete *incompleteMutationError
			if action.Method == pluginMethodFilesystem && pluginWorkspace != "" && errors.As(err, &incomplete) {
				privateRun.Retain()
				err = &incompleteMutationError{err: errors.Join(err, fmt.Errorf("retained plugin workspace at %s", pluginWorkspace))}
			}
			if installed.MutationStarted {
				installedPlugins = append(installedPlugins, installed)
			}
			result.Status = "failed"
			result.Message = err.Error()
			report.Actions = append(report.Actions, result)
			return failPluginQuiescence(err)
		}
		installedPlugins = append(installedPlugins, installed)
		if needsRuntimeRefresh && action.Method == pluginMethodDeckyAPI {
			expected := quiescencePluginNames(quiescence.Inventory)
			if action.Operation == "replace" {
				expected = removePluginName(expected, action.ExistingName)
			}
			expected = append(expected, resolution.StoreName)
			if err := quiescer.SynchronizeQuiescence(ctx, &quiescence, expected); err != nil {
				return failPluginQuiescence(err)
			}
			quiescence.Inventory = replacePluginStatus(quiescence.Inventory, action.ExistingName, resolution.StoreName, resolution.ResolvedVersion)
			transactionMarker.TemporaryDisabledPlugins = append([]string(nil), quiescence.TemporaryDisabled...)
			if err := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); err != nil {
				return failPluginQuiescence(fmt.Errorf("record synchronized Decky quiescence: %w", err))
			}
		}
		result.Status = "applied"
		if action.Operation == "replace" {
			if action.Method == pluginMethodDeckyAPI {
				result.Message = "Previous plugin preserved in the validated recovery snapshot."
			} else {
				result.Message = "Previous plugin preserved at " + action.PreservePath
			}
		}
		report.Actions = append(report.Actions, result)
	}
	if needsRuntimeRefresh {
		// Decky's API mutations may have changed loader.json. Restore the exact
		// guarded pre-transaction file only after all bounded API operations; the
		// running plugin wrappers stay disabled until the final controlled refresh.
		if err := restoreDeckySettingsSideEffects(context.Background(), options.Plan.Target.Home, options.Plan.DeckyLoaderGuard, recoveryStaged, options.Limits); err != nil {
			return failPluginQuiescence(err)
		}
	}
	if planMutatesSteam(options.Plan) {
		steam, supported := options.RuntimeCoordinator.(SteamQuiescer)
		if !supported {
			return failPluginQuiescence(errors.New("restore requires a verified Steam quiescence coordinator before artwork changes"))
		}
		if err := steam.QuiesceSteam(ctx); err != nil {
			return failPluginQuiescence(fmt.Errorf("quiesce Steam before artwork convergence: %w", err))
		}
		transactionMarker.Phase = "steam_quiesced"
		if err := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); err != nil {
			return failPluginQuiescence(fmt.Errorf("record Steam quiescence: %w", err))
		}
	}
	if needsRuntimeRefresh {
		transactionMarker.Phase = "filesystem_convergence"
		if err := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); err != nil {
			return failPluginQuiescence(fmt.Errorf("record filesystem convergence: %w", err))
		}
	}

	applied := make([]Action, 0, len(options.Plan.Actions))
	var applyErr error
	for _, action := range options.Plan.Actions {
		result := ActionResult{LogicalPath: action.LogicalPath, Component: action.Component, Operation: action.Operation}
		if action.Operation == "unchanged" {
			result.Status = "unchanged"
			report.Actions = append(report.Actions, result)
			continue
		}
		if err := ctx.Err(); err != nil {
			applyErr = err
			result.Status = "failed"
			result.Message = err.Error()
			report.Actions = append(report.Actions, result)
			break
		}
		if options.BeforeAction != nil {
			if err := options.BeforeAction(action); err != nil {
				result.Status = "failed"
				result.Message = err.Error()
				report.Actions = append(report.Actions, result)
				applyErr = err
				break
			}
		}
		if action.Operation == "remove" {
			if err := removeAppliedCreate(options.Plan.Target.Home, action); err != nil {
				applyErr = fmt.Errorf("remove %q: %w", action.LogicalPath, err)
				result.Status = "failed"
				result.Message = applyErr.Error()
				report.Actions = append(report.Actions, result)
				break
			}
			if _, err := os.Lstat(action.TargetPath); !errors.Is(err, os.ErrNotExist) {
				applyErr = fmt.Errorf("verify removal %q: target remains present", action.LogicalPath)
				result.Status = "failed"
				result.Message = applyErr.Error()
				report.Actions = append(report.Actions, result)
				applied = append(applied, action)
				break
			}
			applied = append(applied, action)
			result.Status = "applied"
			report.Actions = append(report.Actions, result)
			continue
		}
		payload, exists := staged[action.LogicalPath]
		if !exists {
			applyErr = fmt.Errorf("staged payload is missing for %q", action.LogicalPath)
			result.Status = "failed"
			result.Message = applyErr.Error()
			report.Actions = append(report.Actions, result)
			break
		}
		if err := atomicWrite(ctx, options.Plan.Target.Home, action, payload.Path, os.FileMode(action.DesiredMode), action.Operation); err != nil {
			applyErr = fmt.Errorf("apply %q: %w", action.LogicalPath, err)
			result.Status = "failed"
			result.Message = applyErr.Error()
			report.Actions = append(report.Actions, result)
			var incomplete *incompleteMutationError
			if errors.As(err, &incomplete) {
				applied = append(applied, action)
			}
			break
		}
		if err := verifyAppliedTarget(action); err != nil {
			applyErr = fmt.Errorf("verify %q: %w", action.LogicalPath, err)
			result.Status = "failed"
			result.Message = applyErr.Error()
			report.Actions = append(report.Actions, result)
			applied = append(applied, action)
			break
		}
		applied = append(applied, action)
		result.Status = "applied"
		report.Actions = append(report.Actions, result)
	}
	if applyErr == nil {
		for _, preserved := range options.Plan.PreservedSettings {
			result := ActionResult{LogicalPath: preserved.LogicalPath, Component: "incompatible-settings/" + preserved.Plugin, Operation: "preserve"}
			if preserved.Operation == "unchanged" {
				result.Status = "unchanged"
				result.Message = preserved.Reason
				report.Actions = append(report.Actions, result)
				continue
			}
			if err := ctx.Err(); err != nil {
				applyErr = err
				result.Status = "failed"
				result.Message = err.Error()
				report.Actions = append(report.Actions, result)
				break
			}
			payload, exists := staged[preserved.LogicalPath]
			if !exists {
				applyErr = fmt.Errorf("staged incompatible setting is missing for %q", preserved.LogicalPath)
				result.Status = "failed"
				result.Message = applyErr.Error()
				report.Actions = append(report.Actions, result)
				break
			}
			action := Action{Component: result.Component, LogicalPath: preserved.LogicalPath, TargetRoot: preserved.PreserveRoot, TargetPath: preserved.PreservePath, Operation: "create", Size: preserved.Size, SHA256: preserved.SHA256, DesiredMode: 0o600}
			if err := atomicWrite(ctx, options.Plan.Target.Home, action, payload.Path, 0o600, "create"); err != nil {
				applyErr = fmt.Errorf("preserve incompatible setting %q: %w", preserved.LogicalPath, err)
				result.Status = "failed"
				result.Message = applyErr.Error()
				report.Actions = append(report.Actions, result)
				break
			}
			applied = append(applied, action)
			result.Status = "preserved"
			result.Message = preserved.Reason + " Stored at " + preserved.PreservePath
			report.Actions = append(report.Actions, result)
		}
	}
	if applyErr == nil {
		if err := verifyConverged(ctx, options.Plan, options.Limits, options.DeckyInstaller); err != nil {
			applyErr = fmt.Errorf("verify static converged restore state: %w", err)
		}
		if applyErr == nil && needsRuntimeRefresh {
			transactionMarker.Phase = "static_verification_passed"
			if err := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); err != nil {
				applyErr = fmt.Errorf("record static restore verification: %w", err)
			}
		}
	}
	if applyErr == nil && needsRuntimeRefresh {
		transactionMarker.Phase = "awaiting_reboot"
		if err := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); err != nil {
			applyErr = fmt.Errorf("record pending reboot: %w", err)
		} else if err := options.Rebooter.Request(ctx); err != nil {
			report.Status = "reboot_required"
			report.Error = "Your customization was restored, but the Steam Deck still needs to restart to finish applying it."
			finish()
			_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
			cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
			cleanupStage(runDirectory, stagingDirectory, staged)
			return report, err
		} else {
			report.Status = "awaiting_post_boot_verification"
			finish()
			_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
			cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
			cleanupStage(runDirectory, stagingDirectory, staged)
			return report, nil
		}
	}

	if applyErr != nil {
		report.Error = applyErr.Error()
		fileRollbackErr := rollbackApplied(options.Plan.Target.Home, applied, recoveryStaged)
		pluginRollbackErr := rollbackPlugins()
		rollbackErr := errors.Join(fileRollbackErr, pluginRollbackErr)
		if revalidateErr := revalidate(context.Background(), options.Plan, options.Limits, options.DeckyInstaller); revalidateErr != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("verify rolled-back target state: %w", revalidateErr))
		}
		if rollbackErr != nil {
			report.Status = "rollback_failed"
			report.RollbackError = rollbackErr.Error()
			if needsRuntimeRefresh {
				transactionMarker.Phase = "recovery_required"
				if markerErr := updateIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State, transactionMarker); markerErr != nil {
					report.RollbackError = errors.Join(rollbackErr, fmt.Errorf("record required Decky recovery: %w", markerErr)).Error()
				}
			}
		} else {
			report.Status = "rolled_back"
			if markerErr := removeIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State); markerErr != nil {
				report.Status = "rollback_failed"
				report.RollbackError = markerErr.Error()
			}
		}
		finish()
		_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
		cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
		cleanupStage(runDirectory, stagingDirectory, staged)
		return report, applyErr
	}

	report.Status = "succeeded"
	if err := removeIncompleteTransaction(options.Plan.Target.Home, options.Plan.Target.State); err != nil {
		report.Status = "succeeded_marker_retained"
		report.Error = err.Error()
		finish()
		_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
		return report, err
	}
	finish()
	if err := saveReport(options.Plan.Target.Home, options.ReportDirectory, &report); err != nil {
		report.Status = "succeeded_report_failed"
		report.Error = err.Error()
		cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
		cleanupStage(runDirectory, stagingDirectory, staged)
		return report, err
	}
	cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
	cleanupStage(runDirectory, stagingDirectory, staged)
	return report, nil
}
func planMutatesSteam(plan Plan) bool {
	for _, action := range plan.Actions {
		if action.Component == "steam" && (action.Operation == "create" || action.Operation == "replace" || action.Operation == "remove") {
			return true
		}
	}
	return false
}

func rollbackApplied(home string, applied []Action, recovery map[string]snapshot.StagedFile) error {
	var failures []error
	for index := len(applied) - 1; index >= 0; index-- {
		action := applied[index]
		switch action.Operation {
		case "create":
			if err := removeAppliedCreate(home, action); err != nil {
				failures = append(failures, fmt.Errorf("remove created target %q: %w", action.LogicalPath, err))
			}
		case "replace":
			payload, exists := recovery[action.LogicalPath]
			if !exists {
				failures = append(failures, fmt.Errorf("recovery payload is missing for %q", action.LogicalPath))
				continue
			}
			rollbackAction := action
			rollbackAction.Size = action.ExistingSize
			rollbackAction.SHA256 = action.ExistingSHA256
			rollbackAction.ExistingSize = action.Size
			rollbackAction.ExistingSHA256 = action.SHA256
			rollbackAction.ExistingMode = action.DesiredMode
			if err := atomicWrite(context.Background(), home, rollbackAction, payload.Path, os.FileMode(action.ExistingMode), "replace"); err != nil {
				failures = append(failures, fmt.Errorf("restore recovery payload %q: %w", action.LogicalPath, err))
			}
		case "remove":
			payload, exists := recovery[action.LogicalPath]
			if !exists {
				failures = append(failures, fmt.Errorf("recovery payload is missing for removed target %q", action.LogicalPath))
				continue
			}
			rollbackAction := action
			rollbackAction.Operation = "create"
			rollbackAction.ExistingSize = 0
			rollbackAction.ExistingSHA256 = ""
			rollbackAction.ExistingMode = 0
			if err := atomicWrite(context.Background(), home, rollbackAction, payload.Path, os.FileMode(action.ExistingMode), "create"); err != nil {
				failures = append(failures, fmt.Errorf("restore removed recovery payload %q: %w", action.LogicalPath, err))
			}
		}
	}
	return errors.Join(failures...)
}

func cleanupStage(runDirectory, stageDirectory string, staged map[string]snapshot.StagedFile) {
	// The held privateDirectory handle owns and cleans the entire run tree.
	// Avoid path-based partial cleanup, which could race with replacement.
}

func resolutionForDirectory(resolutions []pluginstore.Resolution, directory string) (pluginstore.Resolution, bool) {
	for _, resolution := range resolutions {
		if resolution.SnapshotDirectory == directory {
			return resolution, true
		}
	}
	return pluginstore.Resolution{}, false
}

func nearestExistingDirectory(path string) (string, error) {
	current := filepath.Clean(path)
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || isLinkOrReparsePoint(info) {
				return "", errors.New("free-space probe is not a real directory")
			}
			return current, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("no existing directory for free-space probe")
		}
		current = parent
	}
}

func saveReport(home, directory string, report *Report) error {
	if err := ensureSecureDirectory(home, directory); err != nil {
		return err
	}
	stamp := strings.NewReplacer(":", "", "-", "", "T", "T").Replace(report.CompletedUTC[:19])
	finalPath := filepath.Join(directory, "restore-report-"+report.PlanID+"-"+stamp+".json")
	finalPath = filepath.Clean(finalPath)
	parent, err := openSecureParent(home, finalPath, false)
	if err != nil {
		return err
	}
	defer parent.root.Close()
	if _, err := parent.root.Lstat(parent.name); err == nil {
		return errors.New("restore report already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	report.ReportPath = finalPath
	contents, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	identifier := make([]byte, 12)
	if _, err := rand.Read(identifier); err != nil {
		return err
	}
	temporaryName := ".restore-report-" + hex.EncodeToString(identifier) + ".tmp"
	temporary, err := parent.root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		temporary.Close()
		if remove {
			_ = parent.root.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := renameNoReplaceRoots(parent.root, temporaryName, parent.root, parent.name); err != nil {
		return err
	}
	remove = false
	return nil
}
