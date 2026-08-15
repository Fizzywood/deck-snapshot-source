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
	Plan              Plan
	ApprovedPlanID    string
	ApprovedHash      string
	Limits            limits.Limits
	WorkDirectory     string
	RecoveryDirectory string
	ReportDirectory   string
	Now               func() time.Time
	AvailableBytes    func(string) (uint64, error)
	BeforeAction      func(Action) error
	BeforePlugin      func(PluginAction) error
	HTTPClient        *http.Client
	DeckyInstaller    deckyapi.Installer
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
	var pluginWorkspace *privateDirectory
	filesystemPlugins := 0
	deckyAPIPlugins := 0
	for _, action := range options.Plan.PluginActions {
		if action.Operation == "create" || action.Operation == "replace" {
			switch action.Method {
			case pluginMethodFilesystem:
				filesystemPlugins++
			case pluginMethodDeckyAPI:
				deckyAPIPlugins++
			}
		}
	}
	if filesystemPlugins > 0 {
		pluginRoot := filepath.Join(options.Plan.Target.Decky, "plugins")
		pluginWorkspace, err = createPrivateDirectory(options.Plan.Target.Home, pluginRoot, ".deck-snapshot-plugin-work-")
		if err != nil {
			finish()
			return report, err
		}
		defer pluginWorkspace.Close()
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
				workspace = filepath.Join(pluginWorkspace.Path, workspaceName)
				if err := pluginWorkspace.root.Mkdir(workspaceName, 0o700); err != nil {
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
		if action.Operation == "replace" {
			recoverySelected = append(recoverySelected, action.LogicalPath)
		}
	}
	if guard := options.Plan.DeckyLoaderGuard; guard != nil && guard.Operation == "restore" {
		recoverySelected = append(recoverySelected, deckyLoaderRecoveryLogicalPath)
	}
	for _, action := range options.Plan.PluginActions {
		if action.Operation != "replace" || action.Method != pluginMethodDeckyAPI {
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
		return rollbackErr
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
			report.Error = err.Error()
			rollbackErr := rollbackPlugins()
			if revalidateErr := revalidate(context.Background(), options.Plan, options.Limits, options.DeckyInstaller); revalidateErr != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("verify rolled-back target state: %w", revalidateErr))
			}
			if rollbackErr != nil {
				report.Status = "rollback_failed"
				report.RollbackError = rollbackErr.Error()
			} else {
				report.Status = "rolled_back"
			}
			finish()
			_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
			return report, err
		}
		if options.BeforePlugin != nil {
			if err := options.BeforePlugin(action); err != nil {
				result.Status = "failed"
				result.Message = err.Error()
				report.Actions = append(report.Actions, result)
				report.Error = err.Error()
				rollbackErr := rollbackPlugins()
				if revalidateErr := revalidate(context.Background(), options.Plan, options.Limits, options.DeckyInstaller); revalidateErr != nil {
					rollbackErr = errors.Join(rollbackErr, fmt.Errorf("verify rolled-back target state: %w", revalidateErr))
				}
				if rollbackErr != nil {
					report.Status = "rollback_failed"
					report.RollbackError = rollbackErr.Error()
				} else {
					report.Status = "rolled_back"
				}
				finish()
				_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
				return report, err
			}
		}
		prepared, exists := preparedPlugins[action.Directory]
		if !exists {
			err := fmt.Errorf("prepared plugin is missing for %q", action.Directory)
			result.Status = "failed"
			result.Message = err.Error()
			report.Actions = append(report.Actions, result)
			finish()
			return report, err
		}
		resolution, resolved := resolutionForDirectory(options.Plan.Plugins, action.Directory)
		if !resolved {
			err := fmt.Errorf("approved plugin resolution is missing for %q", action.Directory)
			result.Status = "failed"
			result.Message = err.Error()
			report.Actions = append(report.Actions, result)
			finish()
			return report, err
		}
		var installed installedPlugin
		if action.Method == pluginMethodDeckyAPI {
			installed, err = installPreparedPluginWithDecky(ctx, options.DeckyInstaller, action, prepared, resolution, options.Limits)
		} else {
			installed, err = installPreparedPlugin(options.Plan.Target.Home, action, prepared, options.Limits)
		}
		if action.Method == pluginMethodDeckyAPI && installed.MutationStarted {
			if sideEffectErr := restoreDeckySettingsSideEffects(context.Background(), options.Plan.Target.Home, options.Plan.DeckyLoaderGuard, recoveryStaged, options.Limits); sideEffectErr != nil {
				err = errors.Join(err, fmt.Errorf("restore Decky Loader settings after bounded plugin operation: %w", sideEffectErr))
			}
		}
		if err != nil {
			var incomplete *incompleteMutationError
			if action.Method == pluginMethodFilesystem && pluginWorkspace != nil && errors.As(err, &incomplete) {
				pluginWorkspace.Retain()
				err = &incompleteMutationError{err: errors.Join(err, fmt.Errorf("retained plugin workspace at %s", pluginWorkspace.Path))}
			}
			if installed.MutationStarted {
				installedPlugins = append(installedPlugins, installed)
			}
			result.Status = "failed"
			result.Message = err.Error()
			report.Actions = append(report.Actions, result)
			report.Error = err.Error()
			rollbackErr := rollbackPlugins()
			if revalidateErr := revalidate(context.Background(), options.Plan, options.Limits, options.DeckyInstaller); revalidateErr != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("verify rolled-back target state: %w", revalidateErr))
			}
			if rollbackErr != nil {
				report.Status = "rollback_failed"
				report.RollbackError = rollbackErr.Error()
			} else {
				report.Status = "rolled_back"
			}
			finish()
			_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
			return report, err
		}
		installedPlugins = append(installedPlugins, installed)
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

	if applyErr != nil {
		report.Error = applyErr.Error()
		pluginRollbackErr := rollbackPlugins()
		fileRollbackErr := rollbackApplied(options.Plan.Target.Home, applied, recoveryStaged)
		rollbackErr := errors.Join(fileRollbackErr, pluginRollbackErr)
		if revalidateErr := revalidate(context.Background(), options.Plan, options.Limits, options.DeckyInstaller); revalidateErr != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("verify rolled-back target state: %w", revalidateErr))
		}
		if rollbackErr != nil {
			report.Status = "rollback_failed"
			report.RollbackError = rollbackErr.Error()
		} else {
			report.Status = "rolled_back"
		}
		finish()
		_ = saveReport(options.Plan.Target.Home, options.ReportDirectory, &report)
		cleanupStage(runDirectory, recoveryStageDirectory, recoveryStaged)
		cleanupStage(runDirectory, stagingDirectory, staged)
		return report, applyErr
	}

	report.Status = "succeeded"
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
