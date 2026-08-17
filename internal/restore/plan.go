// Package restore builds immutable dry-run plans and applies them behind recovery gates.
package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

const PlanVersion = "1.2"

type Plan struct {
	PlanVersion       string                   `json:"plan_version"`
	PlanID            string                   `json:"plan_id"`
	ApprovalHash      string                   `json:"approval_hash"`
	AppVersion        string                   `json:"app_version"`
	CreatedUTC        string                   `json:"created_utc"`
	Snapshot          SnapshotReference        `json:"snapshot"`
	Target            TargetReference          `json:"target"`
	Actions           []Action                 `json:"actions"`
	Plugins           []pluginstore.Resolution `json:"plugins"`
	PluginActions     []PluginAction           `json:"plugin_actions"`
	DeckyLoaderGuard  *LoaderSettingsGuard     `json:"decky_loader_settings_guard,omitempty"`
	PreservedSettings []PreservedSetting       `json:"preserved_incompatible_settings"`
	Warnings          []string                 `json:"warnings"`
	RequiredFreeBytes int64                    `json:"required_free_bytes"`
	TargetFingerprint string                   `json:"target_fingerprint"`
	Blocking          bool                     `json:"blocking"`
}

type SnapshotReference struct {
	Path       string `json:"path"`
	SnapshotID string `json:"snapshot_id"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	CreatedUTC string `json:"created_utc"`
}

type TargetReference struct {
	Home  string `json:"home"`
	Decky string `json:"decky"`
	Steam string `json:"steam"`
	State string `json:"state"`
}

type PluginAction struct {
	Directory           string `json:"directory"`
	Method              string `json:"method,omitempty"`
	TargetRoot          string `json:"target_root"`
	TargetPath          string `json:"target_path"`
	PreserveRoot        string `json:"preserve_root"`
	PreservePath        string `json:"preserve_path"`
	Operation           string `json:"operation"`
	Reason              string `json:"reason,omitempty"`
	ExistingFingerprint string `json:"existing_fingerprint,omitempty"`
	ExistingName        string `json:"existing_name,omitempty"`
	ExistingAuthor      string `json:"existing_author,omitempty"`
	ExistingVersion     string `json:"existing_version,omitempty"`
	ExistingFiles       int    `json:"existing_files,omitempty"`
	ExistingBytes       int64  `json:"existing_bytes,omitempty"`
}

type LoaderSettingsGuard struct {
	TargetRoot     string `json:"target_root"`
	TargetPath     string `json:"target_path"`
	Operation      string `json:"operation"`
	ExistingSize   int64  `json:"existing_size,omitempty"`
	ExistingSHA256 string `json:"existing_sha256,omitempty"`
	ExistingMode   uint32 `json:"existing_mode,omitempty"`
}

type PreservedSetting struct {
	LogicalPath  string `json:"logical_path"`
	Plugin       string `json:"plugin"`
	PreserveRoot string `json:"preserve_root"`
	PreservePath string `json:"preserve_path"`
	Operation    string `json:"operation"`
	Reason       string `json:"reason"`
	Size         int64  `json:"size"`
	SHA256       string `json:"sha256"`
}

type Action struct {
	Component      string `json:"component"`
	LogicalPath    string `json:"logical_path"`
	TargetRoot     string `json:"target_root"`
	TargetPath     string `json:"target_path"`
	Operation      string `json:"operation"`
	Reason         string `json:"reason,omitempty"`
	Size           int64  `json:"size"`
	SHA256         string `json:"sha256"`
	DesiredMode    uint32 `json:"desired_mode"`
	ExistingSize   int64  `json:"existing_size,omitempty"`
	ExistingSHA256 string `json:"existing_sha256,omitempty"`
	ExistingMode   uint32 `json:"existing_mode,omitempty"`
}

type PlanOptions struct {
	Paths          platform.Paths
	SnapshotPath   string
	AppVersion     string
	Now            time.Time
	Limits         limits.Limits
	Resolver       pluginstore.Resolver
	DeckyInstaller deckyapi.Installer
}

// HasMutations reports whether the sealed plan would change supported state.
func (plan Plan) HasMutations() bool {
	for _, action := range plan.Actions {
		if action.Operation == "create" || action.Operation == "replace" || action.Operation == "remove" {
			return true
		}
	}
	for _, action := range plan.PluginActions {
		if action.Operation == "create" || action.Operation == "replace" || action.Operation == "remove" {
			return true
		}
	}
	for _, action := range plan.PreservedSettings {
		if action.Operation == "create" {
			return true
		}
	}
	return false
}

func BuildPlan(ctx context.Context, options PlanOptions) (Plan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.Now.IsZero() {
		options.Now = time.Now()
	}
	if options.AppVersion == "" || !filepath.IsAbs(options.SnapshotPath) {
		return Plan{}, errors.New("app version and an absolute snapshot path are required")
	}
	if err := options.Limits.Validate(); err != nil {
		return Plan{}, fmt.Errorf("validate restore limits: %w", err)
	}
	if err := validateTargetReference(TargetReference{Home: options.Paths.Home, Decky: options.Paths.Decky, Steam: options.Paths.Steam, State: options.Paths.State}); err != nil {
		return Plan{}, err
	}
	value, err := snapshot.ValidateContext(ctx, options.SnapshotPath, options.Limits)
	if err != nil {
		return Plan{}, fmt.Errorf("validate snapshot for restore plan: %w", err)
	}
	snapshotHash, snapshotInfo, err := hashRegularFile(options.SnapshotPath, options.Limits.MaxTotalSize+options.Limits.MaxManifestSize)
	if err != nil {
		return Plan{}, fmt.Errorf("fingerprint snapshot: %w", err)
	}
	plan := Plan{
		PlanVersion: PlanVersion,
		AppVersion:  options.AppVersion,
		CreatedUTC:  options.Now.UTC().Format(time.RFC3339Nano),
		Snapshot: SnapshotReference{
			Path:       options.SnapshotPath,
			SnapshotID: value.SnapshotID,
			Size:       snapshotInfo.Size(),
			SHA256:     snapshotHash,
			CreatedUTC: value.CreatedUTC,
		},
		Target: TargetReference{Home: options.Paths.Home, Decky: options.Paths.Decky, Steam: options.Paths.Steam, State: options.Paths.State},
	}
	for _, file := range value.Files {
		action, mapped, mapErr := buildAction(options.Paths, file, options.Limits)
		if mapErr != nil {
			return Plan{}, mapErr
		}
		if !mapped {
			continue
		}
		plan.Actions = append(plan.Actions, action)
		if action.Operation == "blocked" {
			plan.Blocking = true
		}
	}
	convergence, err := buildConvergenceActions(ctx, options.Paths, value, options.AppVersion, options.Now, options.Limits)
	if err != nil {
		return Plan{}, err
	}
	plan.Actions = append(plan.Actions, convergence...)
	if options.Resolver == nil {
		for _, plugin := range value.Plugins {
			plan.Plugins = append(plan.Plugins, pluginstore.Resolution{SnapshotDirectory: plugin.Directory, SnapshotName: plugin.Name, SnapshotAuthor: plugin.Author, SnapshotVersion: plugin.Version, Status: "resolver_unavailable", Message: "Official current-stable plugin resolution was not available.", Blocking: true})
		}
	} else {
		plan.Plugins, err = options.Resolver.Resolve(ctx, value.Plugins)
		if err != nil {
			for _, plugin := range value.Plugins {
				plan.Plugins = append(plan.Plugins, pluginstore.Resolution{SnapshotDirectory: plugin.Directory, SnapshotName: plugin.Name, SnapshotAuthor: plugin.Author, SnapshotVersion: plugin.Version, Status: "store_unavailable", Message: "The official plugin store could not be verified: " + err.Error(), Blocking: true})
			}
			plan.Warnings = append(plan.Warnings, "Official current-stable plugin resolution failed; the plan is blocked until it can be verified.")
		}
	}
	for _, resolution := range plan.Plugins {
		if resolution.Blocking {
			plan.Blocking = true
		}
	}
	plan.PluginActions, err = buildPluginActions(ctx, options.Paths, value.SnapshotID, plan.Plugins, options.Limits, options.DeckyInstaller)
	if err != nil {
		return Plan{}, err
	}
	bindDeckyAPISettingsRecovery(&plan, options.Paths, options.Limits)
	for _, action := range plan.PluginActions {
		if action.Operation == "blocked" {
			plan.Blocking = true
		}
	}
	plan.Actions, plan.PreservedSettings = splitIncompatibleSettings(options.Paths, value.SnapshotID, plan.Plugins, plan.Actions)
	plan.RequiredFreeBytes = 64 << 20
	if plan.DeckyLoaderGuard != nil && plan.DeckyLoaderGuard.Operation == "restore" {
		if plan.DeckyLoaderGuard.ExistingSize > math.MaxInt64/3 {
			return Plan{}, errors.New("Decky Loader settings recovery space requirement overflow")
		}
		if err := addRequiredBytes(&plan, 3*plan.DeckyLoaderGuard.ExistingSize); err != nil {
			return Plan{}, err
		}
	}
	for _, action := range plan.Actions {
		if action.Operation == "create" || action.Operation == "replace" || action.Operation == "remove" {
			if err := addRequiredBytes(&plan, 2*action.Size+2*action.ExistingSize); err != nil {
				return Plan{}, err
			}
		}
	}
	for _, action := range plan.PreservedSettings {
		if action.Operation == "create" {
			if err := addRequiredBytes(&plan, 2*action.Size); err != nil {
				return Plan{}, err
			}
		}
		if action.Operation == "blocked" {
			plan.Blocking = true
		}
	}
	for _, action := range plan.PluginActions {
		if action.Operation == "create" || action.Operation == "replace" || action.Operation == "remove" {
			existingCopies := action.ExistingBytes
			if action.Method == pluginMethodDeckyAPI {
				if action.ExistingBytes > math.MaxInt64/3 {
					return Plan{}, errors.New("restore plugin space requirement overflow")
				}
				existingCopies = 3 * action.ExistingBytes
			}
			if err := addRequiredBytes(&plan, pluginstore.MaxPackageDownload+pluginstore.MaxPackageTotal+existingCopies); err != nil {
				return Plan{}, err
			}
		}
	}
	plan.Warnings = append(plan.Warnings, "Deck Snapshot refreshes the supported Decky and Steam runtime after a restore; a refresh failure rolls the transaction back.")
	plan.Warnings = append(plan.Warnings, "Non-Steam shortcuts.vdf remains unmodified.")
	sort.Slice(plan.Actions, func(i, j int) bool { return plan.Actions[i].LogicalPath < plan.Actions[j].LogicalPath })
	plan.Blocking = false
	for _, action := range plan.Actions {
		plan.Blocking = plan.Blocking || action.Operation == "blocked"
	}
	for _, resolution := range plan.Plugins {
		plan.Blocking = plan.Blocking || resolution.Blocking
	}
	for _, action := range plan.PluginActions {
		plan.Blocking = plan.Blocking || action.Operation == "blocked"
	}
	for _, action := range plan.PreservedSettings {
		plan.Blocking = plan.Blocking || action.Operation == "blocked"
	}
	plan.TargetFingerprint = fingerprintTargets(plan.Actions, plan.PluginActions, plan.PreservedSettings, plan.DeckyLoaderGuard)
	if err := sealPlan(&plan); err != nil {
		return Plan{}, err
	}
	if err := ValidatePlan(plan); err != nil {
		return Plan{}, fmt.Errorf("validate completed restore plan: %w", err)
	}
	return plan, nil
}

func bindDeckyAPISettingsRecovery(plan *Plan, paths platform.Paths, resourceLimits limits.Limits) {
	deckyAPIMutation := false
	for _, action := range plan.PluginActions {
		deckyAPIMutation = deckyAPIMutation || (action.Method == pluginMethodDeckyAPI && (action.Operation == "create" || action.Operation == "replace" || action.Operation == "remove"))
	}
	if !deckyAPIMutation {
		return
	}
	guard, err := buildDeckyLoaderSettingsGuard(paths, resourceLimits)
	if err != nil {
		for index := range plan.PluginActions {
			action := &plan.PluginActions[index]
			if action.Method == pluginMethodDeckyAPI && (action.Operation == "create" || action.Operation == "replace" || action.Operation == "remove") {
				action.Method = ""
				action.Operation = "blocked"
				action.Reason = "Decky Loader settings side effects cannot be recovered safely: " + err.Error()
			}
		}
		return
	}
	plan.DeckyLoaderGuard = guard
	plan.Warnings = append(plan.Warnings, "Decky Loader plugin installation may update loader settings; the exact pre-operation loader state is recovered immediately after each bounded API operation.")
}

func buildDeckyLoaderSettingsGuard(paths platform.Paths, resourceLimits limits.Limits) (*LoaderSettingsGuard, error) {
	guard := &LoaderSettingsGuard{TargetRoot: filepath.Join(paths.Decky, "settings"), TargetPath: filepath.Join(paths.Decky, "settings", "loader.json")}
	if err := ValidateTarget(guard.TargetRoot, guard.TargetPath); err != nil {
		return nil, err
	}
	rootInfo, err := os.Lstat(guard.TargetRoot)
	if err != nil || !rootInfo.IsDir() || isLinkOrReparsePoint(rootInfo) {
		return nil, errors.New("loader settings parent is not a safe existing directory")
	}
	if err := platformOwnedByCurrentUser(rootInfo); err != nil {
		return nil, fmt.Errorf("loader settings parent ownership is unsafe: %w", err)
	}
	if err := validateWritableAncestor(filepath.Dir(guard.TargetPath)); err != nil {
		return nil, fmt.Errorf("loader settings parent is not writable: %w", err)
	}
	info, err := os.Lstat(guard.TargetPath)
	if errors.Is(err, os.ErrNotExist) {
		guard.Operation = "remove"
		return guard, nil
	}
	if err != nil || !info.Mode().IsRegular() || isLinkOrReparsePoint(info) {
		return nil, errors.New("loader settings target is not a regular file")
	}
	if err := platformOwnedByCurrentUser(info); err != nil {
		return nil, fmt.Errorf("loader settings ownership is unsafe: %w", err)
	}
	checksum, verified, err := hashRegularFile(guard.TargetPath, resourceLimits.MaxFileSize)
	if err != nil {
		return nil, fmt.Errorf("fingerprint loader settings: %w", err)
	}
	guard.Operation = "restore"
	guard.ExistingSize = verified.Size()
	guard.ExistingSHA256 = checksum
	guard.ExistingMode = uint32(verified.Mode().Perm())
	return guard, nil
}

func buildAction(paths platform.Paths, file manifest.File, resourceLimits limits.Limits) (Action, bool, error) {
	root, target, mapped, err := mapTarget(paths, file.LogicalPath)
	if err != nil || !mapped {
		return Action{}, mapped, err
	}
	action := Action{Component: file.Component, LogicalPath: file.LogicalPath, TargetRoot: root, TargetPath: target, Size: file.Size, SHA256: file.SHA256, DesiredMode: 0o600}
	if err := ValidateTarget(root, target); err != nil {
		action.Operation = "blocked"
		action.Reason = err.Error()
		return action, true, nil
	}
	if err := validateWritableAncestor(filepath.Dir(target)); err != nil {
		action.Operation = "blocked"
		action.Reason = "The restore target parent is not writable: " + err.Error()
		return action, true, nil
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		action.Operation = "create"
		return action, true, nil
	}
	if err != nil {
		return Action{}, false, fmt.Errorf("inspect restore target %q: %w", target, err)
	}
	if !info.Mode().IsRegular() || isLinkOrReparsePoint(info) {
		action.Operation = "blocked"
		action.Reason = "The existing target is not a regular file."
		return action, true, nil
	}
	if err := platformOwnedByCurrentUser(info); err != nil {
		action.Operation = "blocked"
		action.Reason = "The existing target ownership is unsafe: " + err.Error()
		return action, true, nil
	}
	hash, verified, err := hashRegularFile(target, resourceLimits.MaxFileSize)
	if err != nil {
		action.Operation = "blocked"
		action.Reason = "The existing target could not be safely fingerprinted: " + err.Error()
		return action, true, nil
	}
	action.ExistingSize = verified.Size()
	action.ExistingSHA256 = hash
	action.ExistingMode = uint32(verified.Mode().Perm())
	if hash == file.SHA256 && verified.Size() == file.Size {
		action.Operation = "unchanged"
	} else {
		action.Operation = "replace"
	}
	return action, true, nil
}

func mapTarget(paths platform.Paths, logicalPath string) (string, string, bool, error) {
	mappings := []struct {
		prefix string
		root   string
	}{
		{prefix: "decky/settings/", root: filepath.Join(paths.Decky, "settings")},
		{prefix: "decky/data/", root: filepath.Join(paths.Decky, "data")},
		{prefix: "css-loader/themes/", root: filepath.Join(paths.Decky, "themes")},
		{prefix: "steam/artwork/librarycache/", root: filepath.Join(paths.Steam, "appcache", "librarycache")},
	}
	for _, mapping := range mappings {
		if strings.HasPrefix(logicalPath, mapping.prefix) {
			relative := strings.TrimPrefix(logicalPath, mapping.prefix)
			return mapping.root, filepath.Join(mapping.root, filepath.FromSlash(relative)), true, nil
		}
	}
	const userdataPrefix = "steam/artwork/userdata/"
	if strings.HasPrefix(logicalPath, userdataPrefix) {
		remainder := strings.TrimPrefix(logicalPath, userdataPrefix)
		parts := strings.Split(remainder, "/")
		if len(parts) < 3 || parts[1] != "grid" || !numeric(parts[0]) {
			return "", "", false, fmt.Errorf("unsupported Steam artwork path %q", logicalPath)
		}
		root := filepath.Join(paths.Steam, "userdata", parts[0], "config", "grid")
		return root, filepath.Join(append([]string{root}, parts[2:]...)...), true, nil
	}
	if strings.HasPrefix(logicalPath, "reports/") {
		return "", "", false, nil
	}
	return "", "", false, fmt.Errorf("snapshot path has no restore mapping: %q", logicalPath)
}

func numeric(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func fingerprintTargets(actions []Action, plugins []PluginAction, preserved []PreservedSetting, guard *LoaderSettingsGuard) string {
	hash := sha256.New()
	for _, action := range actions {
		fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%d\x00%s\x00%d\n", action.LogicalPath, action.TargetPath, action.Operation, action.ExistingSize, action.ExistingSHA256, action.ExistingMode)
	}
	for _, action := range plugins {
		fmt.Fprintf(hash, "plugin\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\n", action.Directory, action.TargetPath, action.PreservePath, action.Operation, action.Method, action.ExistingFingerprint, action.ExistingName, action.ExistingAuthor, action.ExistingVersion, action.ExistingFiles, action.ExistingBytes)
	}
	for _, action := range preserved {
		fmt.Fprintf(hash, "preserve\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s\n", action.LogicalPath, action.Plugin, action.PreservePath, action.Operation, action.Size, action.SHA256)
	}
	if guard != nil {
		fmt.Fprintf(hash, "decky-loader-settings\x00%s\x00%s\x00%s\x00%d\x00%s\x00%d\n", guard.TargetRoot, guard.TargetPath, guard.Operation, guard.ExistingSize, guard.ExistingSHA256, guard.ExistingMode)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func sealPlan(plan *Plan) error {
	plan.PlanID = ""
	plan.ApprovalHash = ""
	contents, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(contents)
	plan.ApprovalHash = hex.EncodeToString(hash[:])
	plan.PlanID = "restore-" + plan.ApprovalHash[:16]
	return nil
}

func ValidatePlan(plan Plan) error {
	if plan.PlanVersion != PlanVersion || plan.PlanID == "" || len(plan.ApprovalHash) != 64 {
		return errors.New("restore plan version or identity is invalid")
	}
	expectedID, expectedHash := plan.PlanID, plan.ApprovalHash
	if err := sealPlan(&plan); err != nil {
		return err
	}
	if plan.PlanID != expectedID || plan.ApprovalHash != expectedHash {
		return errors.New("restore plan approval hash does not match its contents")
	}
	if !filepath.IsAbs(plan.Snapshot.Path) || !validSHA256(plan.Snapshot.SHA256) || plan.Snapshot.Size <= 0 || plan.Snapshot.SnapshotID == "" || plan.TargetFingerprint != fingerprintTargets(plan.Actions, plan.PluginActions, plan.PreservedSettings, plan.DeckyLoaderGuard) {
		return errors.New("restore plan snapshot or target fingerprint is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, plan.CreatedUTC); err != nil {
		return errors.New("restore plan creation time is invalid")
	}
	if err := validateTargetReference(plan.Target); err != nil {
		return err
	}
	paths := platform.Paths{Home: plan.Target.Home, Decky: plan.Target.Decky, Steam: plan.Target.Steam}
	seen := make(map[string]struct{}, len(plan.Actions))
	seenTargets := make(map[string]string, len(plan.Actions)+len(plan.PluginActions)+len(plan.PreservedSettings))
	for _, action := range plan.Actions {
		if err := manifest.ValidateLogicalPath(action.LogicalPath, limits.Default().MaxPathLength); err != nil {
			return err
		}
		if _, duplicate := seen[action.LogicalPath]; duplicate {
			return fmt.Errorf("restore plan contains duplicate action %q", action.LogicalPath)
		}
		seen[action.LogicalPath] = struct{}{}
		if action.Size < 0 || !validSHA256(action.SHA256) || action.DesiredMode != 0o600 {
			return fmt.Errorf("restore action metadata is invalid for %q", action.LogicalPath)
		}
		switch action.Operation {
		case "create":
			if action.ExistingSHA256 != "" || action.ExistingSize != 0 || action.ExistingMode != 0 {
				return fmt.Errorf("create action has existing state for %q", action.LogicalPath)
			}
		case "replace", "unchanged", "remove":
			if action.ExistingSize < 0 || !validSHA256(action.ExistingSHA256) || action.ExistingMode&^0o777 != 0 {
				return fmt.Errorf("existing state is invalid for %q", action.LogicalPath)
			}
			if action.Operation == "remove" && (action.Size != action.ExistingSize || action.SHA256 != action.ExistingSHA256) {
				return fmt.Errorf("remove action identity is inconsistent for %q", action.LogicalPath)
			}
		case "blocked":
			if action.Reason == "" {
				return fmt.Errorf("blocked action has no reason for %q", action.LogicalPath)
			}
		default:
			return fmt.Errorf("unknown restore operation %q", action.Operation)
		}
		mappedRoot, mappedPath, mapped, err := mapTarget(paths, action.LogicalPath)
		if err != nil || !mapped || filepath.Clean(mappedRoot) != filepath.Clean(action.TargetRoot) || filepath.Clean(mappedPath) != filepath.Clean(action.TargetPath) {
			return fmt.Errorf("restore action target does not match its logical path: %q", action.LogicalPath)
		}
		if err := ValidateTarget(action.TargetRoot, action.TargetPath); err != nil {
			return err
		}
		if err := addTargetCollision(seenTargets, action.TargetPath, action.LogicalPath); err != nil {
			return err
		}
	}
	blocking := false
	for _, action := range plan.Actions {
		blocking = blocking || action.Operation == "blocked"
	}
	for _, plugin := range plan.Plugins {
		blocking = blocking || plugin.Blocking
		if plugin.Status == "resolved" && (plugin.Blocking || plugin.StoreID <= 0 || plugin.StoreName == "" || plugin.StoreAuthor == "" || plugin.ResolvedVersion == "" || !validSHA256(plugin.SHA256) || !pluginstore.ValidArtifactURL(plugin.ArtifactURL)) {
			return errors.New("resolved plugin metadata is invalid")
		}
	}
	seenPlugins := make(map[string]struct{}, len(plan.PluginActions))
	deckyAPIMutation := false
	for _, action := range plan.PluginActions {
		blocking = blocking || action.Operation == "blocked"
		if _, exists := seenPlugins[action.Directory]; exists {
			return errors.New("restore plan contains duplicate plugin actions")
		}
		seenPlugins[action.Directory] = struct{}{}
		if err := manifest.ValidateLogicalPath("plugin/"+action.Directory, limits.Default().MaxPathLength); err != nil || strings.ContainsAny(action.Directory, `/\`) {
			return errors.New("restore plugin directory identity is unsafe")
		}
		expectedRoot := filepath.Join(plan.Target.Decky, "plugins")
		expectedPath := filepath.Join(expectedRoot, action.Directory)
		expectedPreserveRoot := filepath.Join(plan.Target.State, "preserved")
		expectedPreservePath := filepath.Join(expectedPreserveRoot, plan.Snapshot.SnapshotID, action.Directory)
		if filepath.Clean(action.TargetRoot) != filepath.Clean(expectedRoot) || filepath.Clean(action.TargetPath) != filepath.Clean(expectedPath) || filepath.Clean(action.PreserveRoot) != filepath.Clean(expectedPreserveRoot) || filepath.Clean(action.PreservePath) != filepath.Clean(expectedPreservePath) {
			return errors.New("restore plugin action target is inconsistent")
		}
		if err := ValidateTarget(action.TargetRoot, action.TargetPath); err != nil {
			return err
		}
		if err := ValidateTarget(action.PreserveRoot, action.PreservePath); err != nil {
			return err
		}
		if err := addTargetCollision(seenTargets, action.TargetPath, "plugin/"+action.Directory); err != nil {
			return err
		}
		if action.Operation == "replace" {
			if err := addTargetCollision(seenTargets, action.PreservePath, "preserved plugin/"+action.Directory); err != nil {
				return err
			}
		}
		switch action.Operation {
		case "create":
			if action.Method != pluginMethodFilesystem && action.Method != pluginMethodDeckyAPI {
				return errors.New("create plugin action has an invalid installation method")
			}
			if action.ExistingFingerprint != "" || action.ExistingFiles != 0 || action.ExistingBytes != 0 {
				return errors.New("create plugin action contains existing state")
			}
			deckyAPIMutation = deckyAPIMutation || action.Method == pluginMethodDeckyAPI
		case "replace":
			if action.Method != pluginMethodFilesystem && action.Method != pluginMethodDeckyAPI {
				return errors.New("replace plugin action has an invalid installation method")
			}
			if !validSHA256(action.ExistingFingerprint) || action.ExistingFiles < 1 || action.ExistingBytes < 0 {
				return errors.New("existing plugin action fingerprint is invalid")
			}
			if action.ExistingVersion == "" {
				return errors.New("replace plugin action version is invalid")
			}
			deckyAPIMutation = deckyAPIMutation || action.Method == pluginMethodDeckyAPI
		case "remove":
			if action.Method != pluginMethodDeckyAPI || !validSHA256(action.ExistingFingerprint) || action.ExistingFiles < 1 || action.ExistingBytes < 0 || action.ExistingName == "" || action.ExistingAuthor == "" || action.ExistingVersion == "" {
				return errors.New("remove plugin action is invalid")
			}
			deckyAPIMutation = true
		case "unchanged":
			if action.Method != pluginMethodNone || !validSHA256(action.ExistingFingerprint) || action.ExistingFiles < 1 || action.ExistingBytes < 0 {
				return errors.New("unchanged plugin action is invalid")
			}
		case "blocked":
			if action.Reason == "" || action.Method != "" {
				return errors.New("blocked plugin action has no reason")
			}
		default:
			return errors.New("unknown restore plugin operation")
		}
	}
	for _, resolution := range plan.Plugins {
		found := false
		for _, action := range plan.PluginActions {
			if action.Directory == resolution.SnapshotDirectory {
				found = true
				break
			}
		}
		if !found {
			return errors.New("restore plugin resolutions and actions differ")
		}
	}
	if deckyAPIMutation {
		guard := plan.DeckyLoaderGuard
		if guard == nil {
			return errors.New("Decky Loader API plugin action lacks a loader settings recovery guard")
		}
		expectedRoot := filepath.Join(plan.Target.Decky, "settings")
		expectedPath := filepath.Join(expectedRoot, "loader.json")
		if filepath.Clean(guard.TargetRoot) != filepath.Clean(expectedRoot) || filepath.Clean(guard.TargetPath) != filepath.Clean(expectedPath) {
			return errors.New("Decky Loader settings recovery guard target is inconsistent")
		}
		if err := ValidateTarget(guard.TargetRoot, guard.TargetPath); err != nil {
			return err
		}
		switch guard.Operation {
		case "remove":
			if guard.ExistingSize != 0 || guard.ExistingSHA256 != "" || guard.ExistingMode != 0 {
				return errors.New("Decky Loader remove guard contains existing state")
			}
		case "restore":
			if guard.ExistingSize < 0 || !validSHA256(guard.ExistingSHA256) || guard.ExistingMode&^0o777 != 0 {
				return errors.New("Decky Loader restore guard identity is invalid")
			}
		default:
			return errors.New("Decky Loader settings recovery guard operation is invalid")
		}
	} else if plan.DeckyLoaderGuard != nil {
		return errors.New("Decky Loader settings recovery guard has no API plugin action")
	}
	seenPreserved := make(map[string]struct{}, len(plan.PreservedSettings))
	for _, action := range plan.PreservedSettings {
		if _, exists := seenPreserved[action.LogicalPath]; exists {
			return errors.New("restore plan contains duplicate preserved settings")
		}
		seenPreserved[action.LogicalPath] = struct{}{}
		if action.Plugin == "" || action.Reason == "" || action.Size < 0 || !validSHA256(action.SHA256) {
			return errors.New("preserved setting metadata is invalid")
		}
		expectedRoot := filepath.Join(plan.Target.State, "incompatible")
		expectedPath := filepath.Join(expectedRoot, plan.Snapshot.SnapshotID, filepath.FromSlash(action.LogicalPath))
		if filepath.Clean(action.PreserveRoot) != filepath.Clean(expectedRoot) || filepath.Clean(action.PreservePath) != filepath.Clean(expectedPath) {
			return errors.New("preserved setting target is inconsistent")
		}
		if err := ValidateTarget(action.PreserveRoot, action.PreservePath); err != nil {
			return err
		}
		if err := addTargetCollision(seenTargets, action.PreservePath, "incompatible/"+action.LogicalPath); err != nil {
			return err
		}
		switch action.Operation {
		case "create", "unchanged":
		case "blocked":
			blocking = true
		default:
			return errors.New("unknown preserved setting operation")
		}
	}
	if plan.Blocking != blocking || plan.RequiredFreeBytes < 64<<20 {
		return errors.New("restore plan blocking or space metadata is inconsistent")
	}
	return nil
}

func validateTargetReference(target TargetReference) error {
	if !filepath.IsAbs(target.Home) || !filepath.IsAbs(target.Decky) || !filepath.IsAbs(target.Steam) || !filepath.IsAbs(target.State) {
		return errors.New("restore target roots must be absolute")
	}
	roots := []string{target.Decky, target.Steam, target.State}
	for _, root := range roots {
		relative, err := filepath.Rel(target.Home, root)
		if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("Decky, Steam and state roots must be beneath the target home")
		}
	}
	for _, root := range roots {
		if err := ValidateTarget(target.Home, filepath.Join(root, ".deck-snapshot-root-scope")); err != nil {
			return fmt.Errorf("restore root has an unsafe existing component: %w", err)
		}
		if err := validateWritableAncestor(root); err != nil {
			return fmt.Errorf("restore root is not writable: %w", err)
		}
		if err := validateOwnedPathBeneathHome(target.Home, root); err != nil {
			return fmt.Errorf("restore root ownership is unsafe: %w", err)
		}
	}
	for first := 0; first < len(roots); first++ {
		for second := first + 1; second < len(roots); second++ {
			firstToSecond, firstErr := filepath.Rel(roots[first], roots[second])
			secondToFirst, secondErr := filepath.Rel(roots[second], roots[first])
			if firstErr != nil || secondErr != nil || firstToSecond == "." || secondToFirst == "." || (!filepath.IsAbs(firstToSecond) && firstToSecond != ".." && !strings.HasPrefix(firstToSecond, ".."+string(filepath.Separator))) || (!filepath.IsAbs(secondToFirst) && secondToFirst != ".." && !strings.HasPrefix(secondToFirst, ".."+string(filepath.Separator))) {
				return errors.New("Decky, Steam and state roots must be distinct and non-overlapping")
			}
		}
	}
	return nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func addRequiredBytes(plan *Plan, amount int64) error {
	if amount < 0 || plan.RequiredFreeBytes > math.MaxInt64-amount {
		return errors.New("restore free-space calculation overflowed")
	}
	plan.RequiredFreeBytes += amount
	return nil
}

func addTargetCollision(seen map[string]string, target, label string) error {
	key := filepath.Clean(target)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	if previous, exists := seen[key]; exists && previous != label {
		return fmt.Errorf("restore targets collide after platform normalization: %q and %q", previous, label)
	}
	seen[key] = label
	return nil
}

func hashRegularFile(path string, maximum int64) (string, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", nil, err
	}
	if !before.Mode().IsRegular() || isLinkOrReparsePoint(before) || before.Size() < 0 || before.Size() > maximum {
		return "", nil, errors.New("file is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return "", nil, errors.New("file identity changed while opening")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil || written != opened.Size() {
		return "", nil, errors.New("file changed while hashing")
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return "", nil, errors.New("file changed while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), opened, nil
}
