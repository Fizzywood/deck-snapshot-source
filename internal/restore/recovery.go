package restore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/discovery"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

const deckyLoaderRecoveryLogicalPath = "recovery/decky-loader-settings/loader.json"

func createRecovery(ctx context.Context, plan Plan, directory string, now time.Time, resourceLimits limits.Limits) (snapshot.Created, error) {
	if !filepath.IsAbs(directory) {
		return snapshot.Created{}, errors.New("recovery directory must be absolute")
	}
	value := manifest.New("recovery-"+plan.PlanID, plan.AppVersion, "recovery", "Restore recovery", now)
	value.Compatibility.UnverifiedBehaviors = []string{"Recovery data is valid only for the exact approved restore plan."}
	result := discovery.Result{Manifest: value}
	for _, action := range plan.Actions {
		switch action.Operation {
		case "replace", "remove":
			if err := ValidateTarget(action.TargetRoot, action.TargetPath); err != nil {
				return snapshot.Created{}, err
			}
			info, err := os.Lstat(action.TargetPath)
			if err != nil || !info.Mode().IsRegular() || isLinkOrReparsePoint(info) {
				return snapshot.Created{}, fmt.Errorf("recovery source is no longer a regular file: %q", action.LogicalPath)
			}
			hash, verified, err := hashRegularFile(action.TargetPath, resourceLimits.MaxFileSize)
			if err != nil || hash != action.ExistingSHA256 || verified.Size() != action.ExistingSize {
				return snapshot.Created{}, fmt.Errorf("recovery source changed after planning: %q", action.LogicalPath)
			}
			entry := manifest.File{
				LogicalPath: action.LogicalPath,
				Component:   "recovery/" + action.Component,
				Size:        verified.Size(),
				SHA256:      hash,
				Mode:        uint32(verified.Mode().Perm()),
			}
			result.Manifest.Files = append(result.Manifest.Files, entry)
			result.Candidates = append(result.Candidates, discovery.Candidate{SourcePath: action.TargetPath, Entry: entry})
		case "create":
			result.Manifest.Exclusions = append(result.Manifest.Exclusions, manifest.Exclusion{LogicalPath: action.LogicalPath, Component: "recovery/" + action.Component, Reason: "target_absent_before_restore"})
		}
	}
	if guard := plan.DeckyLoaderGuard; guard != nil {
		switch guard.Operation {
		case "restore":
			if err := ValidateTarget(guard.TargetRoot, guard.TargetPath); err != nil {
				return snapshot.Created{}, err
			}
			info, err := os.Lstat(guard.TargetPath)
			if err != nil || !info.Mode().IsRegular() || isLinkOrReparsePoint(info) {
				return snapshot.Created{}, errors.New("Decky Loader settings recovery source is no longer a regular file")
			}
			if err := platformOwnedByCurrentUser(info); err != nil {
				return snapshot.Created{}, fmt.Errorf("Decky Loader settings recovery source ownership changed: %w", err)
			}
			hash, verified, err := hashRegularFile(guard.TargetPath, resourceLimits.MaxFileSize)
			if err != nil || hash != guard.ExistingSHA256 || verified.Size() != guard.ExistingSize || uint32(verified.Mode().Perm()) != guard.ExistingMode {
				return snapshot.Created{}, errors.New("Decky Loader settings recovery source changed after planning")
			}
			entry := manifest.File{
				LogicalPath: deckyLoaderRecoveryLogicalPath,
				Component:   "recovery/decky-loader-settings",
				Size:        verified.Size(),
				SHA256:      hash,
				Mode:        uint32(verified.Mode().Perm()),
			}
			result.Manifest.Files = append(result.Manifest.Files, entry)
			result.Candidates = append(result.Candidates, discovery.Candidate{SourcePath: guard.TargetPath, Entry: entry})
		case "remove":
			if err := ValidateTarget(guard.TargetRoot, guard.TargetPath); err != nil {
				return snapshot.Created{}, err
			}
			if _, err := os.Lstat(guard.TargetPath); !errors.Is(err, os.ErrNotExist) {
				if err == nil {
					return snapshot.Created{}, errors.New("Decky Loader settings target appeared after planning")
				}
				return snapshot.Created{}, fmt.Errorf("inspect Decky Loader settings recovery target: %w", err)
			}
			result.Manifest.Exclusions = append(result.Manifest.Exclusions, manifest.Exclusion{LogicalPath: deckyLoaderRecoveryLogicalPath, Component: "recovery/decky-loader-settings", Reason: "target_absent_before_restore"})
		}
	}
	for _, action := range plan.PluginActions {
		if action.Operation == "create" {
			result.Manifest.Exclusions = append(result.Manifest.Exclusions, manifest.Exclusion{LogicalPath: "recovery/plugins/" + action.Directory, Component: "recovery/plugins", Reason: "plugin_absent_before_restore"})
			continue
		}
		if action.Operation != "replace" && action.Operation != "remove" {
			continue
		}
		var fingerprint string
		var files int
		var bytes int64
		var err error
		if action.Method == pluginMethodDeckyAPI {
			fingerprint, files, bytes, err = fingerprintDeckyManagedPluginTree(action.TargetPath, resourceLimits)
		} else {
			fingerprint, files, bytes, err = fingerprintPluginTree(action.TargetPath, resourceLimits)
		}
		if err != nil || fingerprint != action.ExistingFingerprint || files != action.ExistingFiles || bytes != action.ExistingBytes {
			return snapshot.Created{}, fmt.Errorf("plugin recovery source changed after planning: %q", action.Directory)
		}
		err = filepath.WalkDir(action.TargetPath, func(current string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if current == action.TargetPath || entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || isLinkOrReparsePoint(info) {
				return errors.New("plugin recovery source contains an unsafe entry")
			}
			relative, err := filepath.Rel(action.TargetPath, current)
			if err != nil {
				return err
			}
			hash, verified, err := hashRegularFile(current, resourceLimits.MaxFileSize)
			if err != nil {
				return err
			}
			logicalPath := "recovery/plugins/" + action.Directory + "/" + filepath.ToSlash(relative)
			entryValue := manifest.File{LogicalPath: logicalPath, Component: "recovery/plugins", Size: verified.Size(), SHA256: hash, Mode: uint32(verified.Mode().Perm())}
			result.Manifest.Files = append(result.Manifest.Files, entryValue)
			result.Candidates = append(result.Candidates, discovery.Candidate{SourcePath: current, Entry: entryValue})
			return nil
		})
		if err != nil {
			return snapshot.Created{}, err
		}
	}
	result.Manifest.Normalize()
	sort.Slice(result.Candidates, func(i, j int) bool {
		return result.Candidates[i].Entry.LogicalPath < result.Candidates[j].Entry.LogicalPath
	})
	created, err := snapshot.Create(ctx, directory, result, resourceLimits)
	if err != nil {
		return snapshot.Created{}, fmt.Errorf("create recovery snapshot: %w", err)
	}
	validated, err := snapshot.ValidateContext(ctx, created.Path, resourceLimits)
	if err != nil || validated.SnapshotID != result.Manifest.SnapshotID {
		return snapshot.Created{}, errors.New("recovery snapshot failed final validation")
	}
	return created, nil
}

func recoveryManifestsEqual(expected, actual manifest.Manifest) bool {
	return reflect.DeepEqual(expected, actual)
}

func validateStagedRecovery(plan Plan, expected, actual manifest.Manifest, staged map[string]snapshot.StagedFile) error {
	if !recoveryManifestsEqual(expected, actual) {
		return errors.New("staged recovery manifest does not match the created recovery snapshot")
	}
	manifestFiles := make(map[string]manifest.File, len(actual.Files))
	for _, entry := range actual.Files {
		manifestFiles[entry.LogicalPath] = entry
	}
	expectedStaged := make(map[string]manifest.File)
	for _, action := range plan.Actions {
		if action.Operation != "replace" && action.Operation != "remove" {
			continue
		}
		entry, declared := manifestFiles[action.LogicalPath]
		if !declared || entry.Size != action.ExistingSize || entry.SHA256 != action.ExistingSHA256 || entry.Mode != action.ExistingMode {
			return fmt.Errorf("staged recovery payload does not match the approved pre-restore identity: %q", action.LogicalPath)
		}
		expectedStaged[action.LogicalPath] = entry
	}
	if guard := plan.DeckyLoaderGuard; guard != nil && guard.Operation == "restore" {
		entry, declared := manifestFiles[deckyLoaderRecoveryLogicalPath]
		if !declared || entry.Size != guard.ExistingSize || entry.SHA256 != guard.ExistingSHA256 || entry.Mode != guard.ExistingMode {
			return errors.New("staged Decky Loader settings recovery payload does not match the approved pre-restore identity")
		}
		expectedStaged[deckyLoaderRecoveryLogicalPath] = entry
	}
	for _, action := range plan.PluginActions {
		if (action.Operation != "replace" && action.Operation != "remove") || action.Method != pluginMethodDeckyAPI {
			continue
		}
		prefix := "recovery/plugins/" + action.Directory + "/"
		files := 0
		var bytes int64
		for logicalPath, entry := range manifestFiles {
			if !strings.HasPrefix(logicalPath, prefix) {
				continue
			}
			if entry.Mode&0o022 != 0 {
				return fmt.Errorf("plugin recovery payload has unsafe permissions: %q", logicalPath)
			}
			files++
			if entry.Size > math.MaxInt64-bytes {
				return errors.New("plugin recovery byte count overflow")
			}
			bytes += entry.Size
			expectedStaged[logicalPath] = entry
		}
		if files != action.ExistingFiles || bytes != action.ExistingBytes {
			return fmt.Errorf("plugin recovery inventory does not match the approved pre-restore identity: %q", action.Directory)
		}
	}
	for logicalPath, entry := range expectedStaged {
		payload, copied := staged[logicalPath]
		if !copied || payload.Size != entry.Size || payload.SHA256 != entry.SHA256 {
			return fmt.Errorf("staged recovery payload does not match the validated recovery snapshot: %q", logicalPath)
		}
	}
	if len(staged) != len(expectedStaged) {
		return errors.New("staged recovery payload inventory does not match the restore plan")
	}
	return nil
}
