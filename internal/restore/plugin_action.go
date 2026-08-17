package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
)

const (
	pluginMethodNone       = "none"
	pluginMethodFilesystem = "filesystem"
	pluginMethodDeckyAPI   = "decky_loader_v3_2_6"
)

func buildPluginActions(ctx context.Context, paths platform.Paths, snapshotID string, resolutions []pluginstore.Resolution, resourceLimits limits.Limits, installer deckyapi.Installer) ([]PluginAction, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	actions := make([]PluginAction, 0, len(resolutions))
	var probeAttempted bool
	var probeErr error
	probeDecky := func() error {
		if !probeAttempted {
			probeAttempted = true
			if installer == nil {
				probeErr = errors.New("the Decky Loader installation boundary is unavailable")
			} else {
				probeErr = installer.Probe(ctx, paths.Decky)
			}
		}
		return probeErr
	}
	for _, resolution := range resolutions {
		targetRoot := filepath.Join(paths.Decky, "plugins")
		preserveRoot := filepath.Join(paths.State, "preserved")
		action := PluginAction{
			Directory:    resolution.SnapshotDirectory,
			TargetRoot:   targetRoot,
			TargetPath:   filepath.Join(targetRoot, resolution.SnapshotDirectory),
			PreserveRoot: preserveRoot,
			PreservePath: filepath.Join(preserveRoot, snapshotID, resolution.SnapshotDirectory),
		}
		if err := manifest.ValidateLogicalPath("plugin/"+resolution.SnapshotDirectory, resourceLimits.MaxPathLength); err != nil || strings.ContainsAny(resolution.SnapshotDirectory, `/\`) {
			action.Operation = "blocked"
			action.Reason = "The plugin directory identity is unsafe."
			actions = append(actions, action)
			continue
		}
		if resolution.Blocking || resolution.Status != "resolved" {
			action.Operation = "blocked"
			action.Reason = "The current stable plugin identity was not resolved from official metadata."
			actions = append(actions, action)
			continue
		}
		if err := ValidateTarget(action.TargetRoot, action.TargetPath); err != nil {
			action.Operation = "blocked"
			action.Reason = err.Error()
			actions = append(actions, action)
			continue
		}
		if err := ValidateTarget(action.PreserveRoot, action.PreservePath); err != nil {
			action.Operation = "blocked"
			action.Reason = err.Error()
			actions = append(actions, action)
			continue
		}
		info, err := os.Lstat(action.TargetPath)
		if errors.Is(err, os.ErrNotExist) {
			if err := validateWritableAncestor(filepath.Dir(action.PreservePath)); err != nil {
				action.Operation = "blocked"
				action.Reason = "The plugin recovery root is not writable: " + err.Error()
				actions = append(actions, action)
				continue
			}
			action.Operation = "create"
			if err := validateWritableAncestor(action.TargetRoot); err == nil {
				action.Method = pluginMethodFilesystem
			} else if err := probeDecky(); err == nil {
				action.Method = pluginMethodDeckyAPI
			} else {
				action.Operation = "blocked"
				action.Reason = "The plugin target requires a supported Decky Loader installation boundary: " + err.Error()
			}
			actions = append(actions, action)
			continue
		}
		if err != nil || !info.IsDir() || isLinkOrReparsePoint(info) {
			action.Operation = "blocked"
			action.Reason = "The existing plugin target is not a real directory."
			actions = append(actions, action)
			continue
		}
		fingerprint, files, bytes, err := fingerprintDeckyManagedPluginTree(action.TargetPath, resourceLimits)
		if err != nil {
			action.Operation = "blocked"
			action.Reason = "The existing plugin could not be safely fingerprinted: " + err.Error()
			actions = append(actions, action)
			continue
		}
		action.ExistingFingerprint = fingerprint
		action.ExistingFiles = files
		action.ExistingBytes = bytes
		metadata, metadataErr := pluginstore.InspectPackageMetadata(action.TargetPath)
		if metadataErr != nil {
			action.Operation = "blocked"
			action.Reason = "The existing plugin identity could not be verified."
			actions = append(actions, action)
			continue
		}
		action.ExistingVersion = metadata.Version
		if metadata.Name != resolution.StoreName || metadata.Author != resolution.StoreAuthor {
			action.Operation = "blocked"
			action.Reason = "The existing plugin identity does not match the verified official plugin."
			actions = append(actions, action)
			continue
		}
		if metadata.Version == resolution.ResolvedVersion {
			action.Operation = "unchanged"
			action.Method = pluginMethodNone
			actions = append(actions, action)
			continue
		}
		if err := validateWritableAncestor(filepath.Dir(action.PreservePath)); err != nil {
			action.Operation = "blocked"
			action.Reason = "The plugin recovery root is not writable: " + err.Error()
			actions = append(actions, action)
			continue
		}
		action.Operation = "replace"
		_, _, _, currentOwnerErr := fingerprintPluginTree(action.TargetPath, resourceLimits)
		if writableErr := validateWritableAncestor(action.TargetRoot); writableErr == nil && currentOwnerErr == nil {
			if _, err := os.Lstat(action.PreservePath); err == nil {
				action.Operation = "blocked"
				action.Reason = "The plugin preservation target already exists."
			} else if !errors.Is(err, os.ErrNotExist) {
				action.Operation = "blocked"
				action.Reason = "The plugin preservation target could not be inspected."
			} else {
				action.Method = pluginMethodFilesystem
			}
		} else if err := probeDecky(); err != nil {
			action.Operation = "blocked"
			action.Reason = "The plugin target requires a supported Decky Loader installation boundary: " + err.Error()
		} else {
			action.Method = pluginMethodDeckyAPI
		}
		actions = append(actions, action)
	}
	// Only positively identified, real plugin directories are candidates for
	// convergence removal. Unknown files and stale settings/data roots are not
	// considered here and are deliberately left alone.
	snapshotDirectories := make(map[string]struct{}, len(resolutions))
	for _, resolution := range resolutions {
		snapshotDirectories[resolution.SnapshotDirectory] = struct{}{}
	}
	pluginRoot := filepath.Join(paths.Decky, "plugins")
	rootInfo, rootErr := os.Lstat(pluginRoot)
	if errors.Is(rootErr, os.ErrNotExist) {
		sort.Slice(actions, func(i, j int) bool { return actions[i].Directory < actions[j].Directory })
		return actions, nil
	}
	if rootErr != nil || !rootInfo.IsDir() || isLinkOrReparsePoint(rootInfo) {
		return nil, errors.New("Decky plugin root is not a real directory")
	}
	entries, readErr := os.ReadDir(pluginRoot)
	if readErr != nil {
		return nil, fmt.Errorf("read Decky plugin root: %w", readErr)
	}
	// Decky's uninstall route accepts the display name, not a filesystem path.
	// Count only fully parseable identities first so an action is never emitted
	// when two live directories could resolve to the same uninstall target.
	nameCounts := make(map[string]int)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		metadata, err := pluginstore.InspectPackageMetadata(filepath.Join(pluginRoot, entry.Name()))
		if err == nil && metadata.Name != "" && metadata.Author != "" && metadata.Version != "" {
			nameCounts[metadata.Name]++
		}
	}
	for _, entry := range entries {
		if _, present := snapshotDirectories[entry.Name()]; present {
			continue
		}
		directory := entry.Name()
		action := PluginAction{Directory: directory, TargetRoot: pluginRoot, TargetPath: filepath.Join(pluginRoot, directory), PreserveRoot: filepath.Join(paths.State, "preserved"), PreservePath: filepath.Join(paths.State, "preserved", snapshotID, directory)}
		if err := manifest.ValidateLogicalPath("plugin/"+directory, resourceLimits.MaxPathLength); err != nil || strings.ContainsAny(directory, `/\`) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			action.Operation = "blocked"
			action.Reason = "An extra plugin is not a safely identified real plugin directory."
			actions = append(actions, action)
			continue
		}
		fingerprint, files, bytes, err := fingerprintDeckyManagedPluginTree(action.TargetPath, resourceLimits)
		if err != nil {
			action.Operation = "blocked"
			action.Reason = "The extra plugin could not be safely fingerprinted."
			actions = append(actions, action)
			continue
		}
		metadata, err := pluginstore.InspectPackageMetadata(action.TargetPath)
		if err != nil || metadata.Name == "" || metadata.Author == "" || metadata.Version == "" {
			action.Operation = "blocked"
			action.Reason = "The extra plugin identity could not be verified."
			actions = append(actions, action)
			continue
		}
		if nameCounts[metadata.Name] != 1 {
			action.Operation = "blocked"
			action.Reason = "The extra plugin display name is ambiguous, so Decky Loader removal is unsafe."
			actions = append(actions, action)
			continue
		}
		if err := probeDecky(); err != nil {
			action.Operation = "blocked"
			action.Reason = "The extra plugin cannot be removed through the supported Decky Loader boundary."
			actions = append(actions, action)
			continue
		}
		action.Method = pluginMethodDeckyAPI
		action.Operation = "remove"
		action.ExistingFingerprint = fingerprint
		action.ExistingFiles = files
		action.ExistingBytes = bytes
		action.ExistingName = metadata.Name
		action.ExistingAuthor = metadata.Author
		action.ExistingVersion = metadata.Version
		actions = append(actions, action)
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i].Directory < actions[j].Directory })
	return actions, nil
}

func fingerprintPluginTree(root string, resourceLimits limits.Limits) (string, int, int64, error) {
	return fingerprintPluginTreeWithOwner(root, resourceLimits, platformOwnedByCurrentUser, false, true)
}

func fingerprintDeckyManagedPluginTree(root string, resourceLimits limits.Limits) (string, int, int64, error) {
	return fingerprintPluginTreeWithOwner(root, resourceLimits, platformDeckyManagedOwner, true, true)
}

func fingerprintPreparedPluginContent(root string, resourceLimits limits.Limits) (string, int, int64, error) {
	return fingerprintPluginTreeWithOwner(root, resourceLimits, platformOwnedByCurrentUser, false, false)
}

func fingerprintDeckyManagedPluginContent(root string, resourceLimits limits.Limits) (string, int, int64, error) {
	return fingerprintPluginTreeWithOwner(root, resourceLimits, platformDeckyManagedOwner, true, false)
}

func fingerprintPluginTreeWithOwner(root string, resourceLimits limits.Limits, validateOwner func(os.FileInfo) error, rejectSharedWrites, includeModes bool) (string, int, int64, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || isLinkOrReparsePoint(rootInfo) {
		return "", 0, 0, errors.New("plugin root is not a real directory")
	}
	if err := validateOwner(rootInfo); err != nil {
		return "", 0, 0, err
	}
	if rejectSharedWrites {
		if err := platformDeckyManagedMode(rootInfo); err != nil {
			return "", 0, 0, fmt.Errorf("plugin root permissions are unsafe: %w", err)
		}
	}
	hash := sha256.New()
	counter := limits.Counter{Limits: resourceLimits}
	err = filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		logical := "plugin/" + filepath.ToSlash(relative)
		if err := manifest.ValidateLogicalPath(logical, resourceLimits.MaxPathLength); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if isLinkOrReparsePoint(info) {
			return fmt.Errorf("plugin path is a symlink: %s", logical)
		}
		if err := validateOwner(info); err != nil {
			return err
		}
		if rejectSharedWrites {
			if err := platformDeckyManagedMode(info); err != nil {
				return fmt.Errorf("plugin path permissions are unsafe: %s: %w", logical, err)
			}
		}
		if info.IsDir() {
			fmt.Fprintf(hash, "d\x00%s\n", logical)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("plugin path is not a regular file: %s", logical)
		}
		if err := counter.Add(info.Size()); err != nil {
			return err
		}
		fileHash, verified, err := hashRegularFile(current, resourceLimits.MaxFileSize)
		if err != nil || verified.Size() != info.Size() {
			return errors.New("plugin file changed while fingerprinting")
		}
		if includeModes {
			fmt.Fprintf(hash, "f\x00%s\x00%d\x00%d\x00%s\n", logical, verified.Size(), verified.Mode().Perm(), fileHash)
		} else {
			fmt.Fprintf(hash, "f\x00%s\x00%d\x00%s\n", logical, verified.Size(), fileHash)
		}
		return nil
	})
	if err != nil {
		return "", 0, 0, err
	}
	if counter.Files == 0 {
		return "", 0, 0, errors.New("plugin directory contains no regular files")
	}
	return hex.EncodeToString(hash.Sum(nil)), counter.Files, counter.Bytes, nil
}
