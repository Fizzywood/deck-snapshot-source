package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
)

type installedPlugin struct {
	Action          PluginAction
	NewFingerprint  string
	NewFiles        int
	NewBytes        int64
	NewContent      string
	Resolution      pluginstore.Resolution
	MutationStarted bool
}

func installPreparedPlugin(home string, action PluginAction, prepared pluginstore.PreparedPackage, resourceLimits limits.Limits) (installedPlugin, error) {
	if action.Method != pluginMethodFilesystem {
		return installedPlugin{}, errors.New("plugin action does not use the filesystem installation method")
	}
	if filepath.Clean(prepared.Root) == filepath.Clean(action.TargetPath) {
		return installedPlugin{}, errors.New("plugin staging path aliases the production target")
	}
	newFingerprint, newFiles, newBytes, err := fingerprintPluginTree(prepared.Root, resourceLimits)
	if err != nil {
		return installedPlugin{}, fmt.Errorf("fingerprint prepared plugin: %w", err)
	}
	if prepared.Directory != action.Directory {
		return installedPlugin{}, errors.New("prepared plugin directory does not match the plan")
	}
	targetParent, err := openSecureParent(home, action.TargetPath, true)
	if err != nil {
		return installedPlugin{}, err
	}
	targetParent.root.Close()
	if action.Operation == "replace" {
		preserveParent, err := openSecureParent(home, action.PreservePath, true)
		if err != nil {
			return installedPlugin{}, err
		}
		preserveParent.root.Close()
	}

	if _, err := relativeToHome(home, prepared.Root); err != nil {
		return installedPlugin{}, errors.New("prepared plugin is not on the target-home filesystem")
	}
	item := installedPlugin{Action: action, NewFingerprint: newFingerprint, NewFiles: newFiles, NewBytes: newBytes}

	switch action.Operation {
	case "create":
		if _, err := os.Lstat(action.TargetPath); !errors.Is(err, os.ErrNotExist) {
			return installedPlugin{}, errors.New("plugin target appeared after approval")
		}
		if err := moveNoReplace(home, prepared.Root, action.TargetPath, false); err != nil {
			var committed *namespaceMutationError
			if errors.As(err, &committed) {
				item.MutationStarted = true
				return item, fmt.Errorf("publish plugin directory without replacement: %w", err)
			}
			return installedPlugin{}, fmt.Errorf("publish plugin directory without replacement: %w", err)
		}
		item.MutationStarted = true
	case "replace":
		currentFingerprint, currentFiles, currentBytes, err := fingerprintPluginTree(action.TargetPath, resourceLimits)
		if err != nil || currentFingerprint != action.ExistingFingerprint || currentFiles != action.ExistingFiles || currentBytes != action.ExistingBytes {
			return installedPlugin{}, errors.New("existing plugin changed after approval")
		}
		if _, err := os.Lstat(action.PreservePath); !errors.Is(err, os.ErrNotExist) {
			return installedPlugin{}, errors.New("plugin preservation target appeared after approval")
		}
		if err := exchangePluginPaths(home, prepared.Root, action.TargetPath); err != nil {
			var committed *namespaceMutationError
			if errors.As(err, &committed) {
				item.MutationStarted = true
				return item, &incompleteMutationError{err: fmt.Errorf("atomically exchange replacement plugin: %w", err)}
			}
			return installedPlugin{}, fmt.Errorf("atomically exchange replacement plugin: %w", err)
		}
		item.MutationStarted = true
		preservedFingerprint, preservedFiles, preservedBytes, verifyErr := fingerprintPluginTree(prepared.Root, resourceLimits)
		if verifyErr != nil || preservedFingerprint != action.ExistingFingerprint || preservedFiles != action.ExistingFiles || preservedBytes != action.ExistingBytes {
			restoreErr := exchangePluginPaths(home, prepared.Root, action.TargetPath)
			if restoreErr != nil {
				return item, &incompleteMutationError{err: errors.Join(errors.New("displaced plugin did not match the approved identity"), verifyErr, restoreErr)}
			}
			reversedFingerprint, reversedFiles, reversedBytes, reversedErr := fingerprintPluginTree(prepared.Root, resourceLimits)
			if reversedErr != nil || reversedFingerprint != newFingerprint || reversedFiles != newFiles || reversedBytes != newBytes {
				return installedPlugin{}, &incompleteMutationError{err: errors.Join(fmt.Errorf("plugin exchange reversal retained an unverified private tree at %s", prepared.Root), verifyErr, reversedErr)}
			}
			return installedPlugin{}, errors.New("existing plugin changed during atomic exchange")
		}
		if err := moveNoReplace(home, prepared.Root, action.PreservePath, true); err != nil {
			var committed *namespaceMutationError
			if errors.As(err, &committed) {
				return item, &incompleteMutationError{err: fmt.Errorf("preserve displaced plugin durably: %w", err)}
			}
			restoreErr := exchangePluginPaths(home, prepared.Root, action.TargetPath)
			if restoreErr != nil {
				return item, &incompleteMutationError{err: errors.Join(fmt.Errorf("preserve displaced plugin: %w", err), restoreErr)}
			}
			reversedFingerprint, reversedFiles, reversedBytes, reversedErr := fingerprintPluginTree(prepared.Root, resourceLimits)
			if reversedErr != nil || reversedFingerprint != newFingerprint || reversedFiles != newFiles || reversedBytes != newBytes {
				return installedPlugin{}, &incompleteMutationError{err: errors.Join(fmt.Errorf("plugin preservation reversal retained an unverified private tree at %s", prepared.Root), err, reversedErr)}
			}
			return installedPlugin{}, fmt.Errorf("preserve displaced plugin: %w", err)
		}
	default:
		return installedPlugin{}, errors.New("plugin install operation is not mutable")
	}
	installedFingerprint, installedFiles, installedBytes, err := fingerprintPluginTree(action.TargetPath, resourceLimits)
	if err != nil || installedFingerprint != newFingerprint || installedFiles != newFiles || installedBytes != newBytes {
		verifyErr := errors.New("installed plugin failed final verification")
		transaction, transactionErr := createPrivateDirectory(home, filepath.Dir(action.TargetPath), ".deck-snapshot-plugin-install-")
		if transactionErr != nil {
			return item, &incompleteMutationError{err: errors.Join(verifyErr, err, transactionErr)}
		}
		if action.Operation == "replace" {
			exchangeErr := exchangePluginPaths(home, action.TargetPath, action.PreservePath)
			if exchangeErr != nil {
				failure := retainIncomplete(transaction, errors.Join(verifyErr, err, exchangeErr))
				_ = transaction.Close()
				return item, failure
			}
			restoredFingerprint, restoredFiles, restoredBytes, restoredErr := fingerprintPluginTree(action.TargetPath, resourceLimits)
			quarantineErr := moveNoReplace(home, action.PreservePath, filepath.Join(transaction.Path, "failed"), false)
			failedFingerprint, failedFiles, failedBytes, failedErr := fingerprintPluginTree(filepath.Join(transaction.Path, "failed"), resourceLimits)
			if restoredErr != nil || restoredFingerprint != action.ExistingFingerprint || restoredFiles != action.ExistingFiles || restoredBytes != action.ExistingBytes || quarantineErr != nil || failedErr != nil || failedFingerprint != newFingerprint || failedFiles != newFiles || failedBytes != newBytes {
				failure := retainIncomplete(transaction, errors.Join(verifyErr, err, restoredErr, quarantineErr, failedErr))
				_ = transaction.Close()
				return item, failure
			}
		} else {
			failedPath := filepath.Join(transaction.Path, "failed")
			moveBackErr := moveNoReplace(home, action.TargetPath, failedPath, false)
			failedFingerprint, failedFiles, failedBytes, failedVerifyErr := fingerprintPluginTree(failedPath, resourceLimits)
			failedIsGenerated := moveBackErr == nil && failedVerifyErr == nil && failedFingerprint == newFingerprint && failedFiles == newFiles && failedBytes == newBytes
			if !failedIsGenerated {
				restoreErr := moveNoReplace(home, failedPath, action.TargetPath, false)
				failure := retainIncomplete(transaction, errors.Join(verifyErr, err, moveBackErr, failedVerifyErr, restoreErr))
				_ = transaction.Close()
				return item, failure
			}
		}
		if closeErr := transaction.Close(); closeErr != nil {
			failure := &incompleteMutationError{err: errors.Join(verifyErr, err, closeErr)}
			return item, failure
		}
		return installedPlugin{}, errors.Join(verifyErr, err)
	}
	return item, nil
}

func installPreparedPluginWithDecky(ctx context.Context, installer deckyapi.Installer, action PluginAction, prepared pluginstore.PreparedPackage, resolution pluginstore.Resolution, resourceLimits limits.Limits) (installedPlugin, error) {
	if action.Method != pluginMethodDeckyAPI || installer == nil {
		return installedPlugin{}, errors.New("Decky Loader installation boundary is unavailable")
	}
	if prepared.Directory != action.Directory || prepared.Archive == "" || prepared.Metadata.Name != resolution.StoreName || prepared.Metadata.Author != resolution.StoreAuthor || prepared.Metadata.Version != resolution.ResolvedVersion {
		return installedPlugin{}, errors.New("prepared plugin identity does not match the approved Decky Loader request")
	}
	archiveHash, archiveInfo, archiveErr := hashRegularFile(prepared.Archive, pluginstore.MaxPackageDownload)
	if archiveErr != nil || archiveInfo.Size() < 1 || archiveHash != resolution.SHA256 {
		return installedPlugin{}, errors.Join(errors.New("prepared plugin archive does not match the approved checksum"), archiveErr)
	}
	newContent, newFiles, newBytes, err := fingerprintPreparedPluginContent(prepared.Root, resourceLimits)
	if err != nil {
		return installedPlugin{}, fmt.Errorf("fingerprint prepared plugin content: %w", err)
	}
	item := installedPlugin{Action: action, NewContent: newContent, NewFiles: newFiles, NewBytes: newBytes, Resolution: resolution}
	switch action.Operation {
	case "create":
		if _, err := os.Lstat(action.TargetPath); !errors.Is(err, os.ErrNotExist) {
			return installedPlugin{}, errors.New("plugin target appeared after approval")
		}
	case "replace":
		fingerprint, files, bytes, err := fingerprintDeckyManagedPluginTree(action.TargetPath, resourceLimits)
		if err != nil || fingerprint != action.ExistingFingerprint || files != action.ExistingFiles || bytes != action.ExistingBytes {
			return installedPlugin{}, errors.New("existing Decky-managed plugin changed after approval")
		}
	default:
		return installedPlugin{}, errors.New("plugin install operation is not mutable")
	}
	item.MutationStarted = true
	if err := installer.Install(ctx, deckyapi.InstallRequest{ArchivePath: prepared.Archive, Name: resolution.StoreName, Version: resolution.ResolvedVersion, SHA256: resolution.SHA256, Replace: action.Operation == "replace"}); err != nil {
		return item, fmt.Errorf("install plugin through Decky Loader: %w", err)
	}
	metadata, metadataErr := pluginstore.InspectPackageMetadata(action.TargetPath)
	installedContent, installedFiles, installedBytes, fingerprintErr := fingerprintDeckyManagedPluginContent(action.TargetPath, resourceLimits)
	installedFingerprint, _, _, exactErr := fingerprintDeckyManagedPluginTree(action.TargetPath, resourceLimits)
	if metadataErr != nil || metadata.Name != resolution.StoreName || metadata.Author != resolution.StoreAuthor || metadata.Version != resolution.ResolvedVersion || fingerprintErr != nil || exactErr != nil || installedContent != newContent || installedFiles != newFiles || installedBytes != newBytes {
		return item, &incompleteMutationError{err: errors.Join(errors.New("Decky Loader installed plugin failed final identity verification"), metadataErr, fingerprintErr, exactErr)}
	}
	item.NewFingerprint = installedFingerprint
	return item, nil
}

func rollbackInstalledPlugins(home string, installed []installedPlugin, resourceLimits limits.Limits) error {
	return rollbackInstalledPluginsWithDecky(context.Background(), home, installed, resourceLimits, nil, nil)
}

func rollbackInstalledPluginsWithDecky(ctx context.Context, home string, installed []installedPlugin, resourceLimits limits.Limits, installer deckyapi.Installer, recoveryPackages map[string]recoveryPluginPackage) error {
	var failures []error
	for index := len(installed) - 1; index >= 0; index-- {
		item := installed[index]
		if item.Action.Method == pluginMethodDeckyAPI {
			if err := rollbackDeckyInstalledPlugin(ctx, item, resourceLimits, installer, recoveryPackages[item.Action.Directory]); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		info, statErr := os.Lstat(item.Action.TargetPath)
		if errors.Is(statErr, os.ErrNotExist) {
			if item.Action.Operation == "replace" {
				if err := moveNoReplace(home, item.Action.PreservePath, item.Action.TargetPath, false); err != nil {
					failures = append(failures, fmt.Errorf("restore absent plugin target %q: %w", item.Action.Directory, err))
				} else {
					fingerprint, files, bytes, verifyErr := fingerprintPluginTree(item.Action.TargetPath, resourceLimits)
					if verifyErr != nil || fingerprint != item.Action.ExistingFingerprint || files != item.Action.ExistingFiles || bytes != item.Action.ExistingBytes {
						transaction, transactionErr := createPrivateDirectory(home, filepath.Dir(item.Action.TargetPath), ".deck-snapshot-plugin-rollback-")
						if transactionErr != nil {
							failures = append(failures, errors.Join(errors.New("restored plugin identity is uncertain"), verifyErr, transactionErr))
							continue
						}
						quarantineErr := moveNoReplace(home, item.Action.TargetPath, filepath.Join(transaction.Path, "unexpected-previous"), false)
						failure := retainIncomplete(transaction, errors.Join(errors.New("restored plugin identity is uncertain"), verifyErr, quarantineErr))
						_ = transaction.Close()
						failures = append(failures, failure)
					}
				}
			}
			continue
		}
		if statErr != nil || !info.IsDir() || isLinkOrReparsePoint(info) {
			failures = append(failures, fmt.Errorf("installed plugin target is unsafe during rollback: %q", item.Action.Directory))
			continue
		}
		if item.Action.Operation == "replace" {
			if err := rollbackReplacedPlugin(home, item, resourceLimits); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		transaction, err := createPrivateDirectory(home, filepath.Dir(item.Action.TargetPath), ".deck-snapshot-plugin-rollback-")
		if err != nil {
			failures = append(failures, err)
			continue
		}
		quarantinePath := filepath.Join(transaction.Path, "installed")
		if err := moveNoReplace(home, item.Action.TargetPath, quarantinePath, false); err != nil {
			_ = transaction.Close()
			failures = append(failures, fmt.Errorf("quarantine plugin during rollback %q: %w", item.Action.Directory, err))
			continue
		}
		currentFingerprint, currentFiles, currentBytes, verifyErr := fingerprintPluginTree(quarantinePath, resourceLimits)
		if verifyErr != nil || currentFingerprint != item.NewFingerprint || currentFiles != item.NewFiles || currentBytes != item.NewBytes {
			restoreConcurrentErr := moveNoReplace(home, quarantinePath, item.Action.TargetPath, false)
			failure := retainIncomplete(transaction, errors.Join(fmt.Errorf("installed plugin changed during rollback: %q", item.Action.Directory), verifyErr, restoreConcurrentErr))
			_ = transaction.Close()
			failures = append(failures, failure)
			continue
		}
		if err := transaction.Close(); err != nil {
			failures = append(failures, fmt.Errorf("clean verified plugin rollback transaction %q: %w", item.Action.Directory, err))
		}
	}
	return errors.Join(failures...)
}

func rollbackDeckyInstalledPlugin(ctx context.Context, item installedPlugin, resourceLimits limits.Limits, installer deckyapi.Installer, recovery recoveryPluginPackage) error {
	if installer == nil {
		return fmt.Errorf("Decky Loader rollback boundary is unavailable for %q", item.Action.Directory)
	}
	info, statErr := os.Lstat(item.Action.TargetPath)
	if item.Action.Operation == "create" {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil || !info.IsDir() || isLinkOrReparsePoint(info) {
			return fmt.Errorf("created Decky plugin target is unsafe during rollback: %q", item.Action.Directory)
		}
		if err := verifyGeneratedDeckyPlugin(item, resourceLimits); err != nil {
			return err
		}
		if err := installer.Uninstall(ctx, item.Resolution.StoreName); err != nil {
			return fmt.Errorf("uninstall created plugin through Decky Loader %q: %w", item.Action.Directory, err)
		}
		if _, err := os.Lstat(item.Action.TargetPath); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("Decky Loader did not remove the created plugin during rollback: %q", item.Action.Directory)
		}
		return nil
	}
	if item.Action.Operation != "replace" || recovery.Archive == "" || !validSHA256(recovery.SHA256) {
		return fmt.Errorf("validated plugin recovery package is unavailable for %q", item.Action.Directory)
	}
	replace := false
	if statErr == nil {
		if !info.IsDir() || isLinkOrReparsePoint(info) {
			return fmt.Errorf("replacement Decky plugin target is unsafe during rollback: %q", item.Action.Directory)
		}
		currentFingerprint, currentFiles, currentBytes, currentErr := fingerprintDeckyManagedPluginTree(item.Action.TargetPath, resourceLimits)
		if currentErr == nil && currentFingerprint == item.Action.ExistingFingerprint && currentFiles == item.Action.ExistingFiles && currentBytes == item.Action.ExistingBytes {
			return nil
		}
		if err := verifyGeneratedDeckyPlugin(item, resourceLimits); err != nil {
			return err
		}
		replace = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect Decky plugin target during rollback %q: %w", item.Action.Directory, statErr)
	}
	if err := installer.Install(ctx, deckyapi.InstallRequest{ArchivePath: recovery.Archive, Name: item.Resolution.StoreName, Version: item.Action.ExistingVersion, SHA256: recovery.SHA256, Replace: replace}); err != nil {
		return fmt.Errorf("restore plugin through Decky Loader %q: %w", item.Action.Directory, err)
	}
	fingerprint, files, bytes, verifyErr := fingerprintDeckyManagedPluginTree(item.Action.TargetPath, resourceLimits)
	metadata, metadataErr := pluginstore.InspectPackageMetadata(item.Action.TargetPath)
	if verifyErr != nil || metadataErr != nil || fingerprint != item.Action.ExistingFingerprint || files != item.Action.ExistingFiles || bytes != item.Action.ExistingBytes || metadata.Name != item.Resolution.StoreName || metadata.Author != item.Resolution.StoreAuthor || metadata.Version != item.Action.ExistingVersion {
		return errors.Join(fmt.Errorf("Decky Loader rollback failed final identity verification for %q", item.Action.Directory), verifyErr, metadataErr)
	}
	return nil
}

func verifyGeneratedDeckyPlugin(item installedPlugin, resourceLimits limits.Limits) error {
	metadata, metadataErr := pluginstore.InspectPackageMetadata(item.Action.TargetPath)
	if metadataErr != nil || metadata.Name != item.Resolution.StoreName || metadata.Author != item.Resolution.StoreAuthor || metadata.Version != item.Resolution.ResolvedVersion {
		return errors.Join(fmt.Errorf("Decky-managed plugin identity changed before rollback: %q", item.Action.Directory), metadataErr)
	}
	if item.NewFingerprint != "" {
		fingerprint, files, bytes, err := fingerprintDeckyManagedPluginTree(item.Action.TargetPath, resourceLimits)
		if err != nil || fingerprint != item.NewFingerprint || files != item.NewFiles || bytes != item.NewBytes {
			return errors.Join(fmt.Errorf("Decky-managed plugin changed before rollback: %q", item.Action.Directory), err)
		}
		return nil
	}
	content, files, bytes, err := fingerprintDeckyManagedPluginContent(item.Action.TargetPath, resourceLimits)
	if err != nil || content != item.NewContent || files != item.NewFiles || bytes != item.NewBytes {
		return errors.Join(fmt.Errorf("Decky-managed plugin content is uncertain before rollback: %q", item.Action.Directory), err)
	}
	return nil
}

func rollbackReplacedPlugin(home string, item installedPlugin, resourceLimits limits.Limits) error {
	transaction, err := createPrivateDirectory(home, filepath.Dir(item.Action.TargetPath), ".deck-snapshot-plugin-rollback-")
	if err != nil {
		return err
	}
	if err := exchangePluginPaths(home, item.Action.TargetPath, item.Action.PreservePath); err != nil {
		var committed *namespaceMutationError
		if errors.As(err, &committed) {
			failure := retainIncomplete(transaction, fmt.Errorf("plugin rollback exchange durability is uncertain for %q: %w", item.Action.Directory, err))
			_ = transaction.Close()
			return failure
		}
		_ = transaction.Close()
		return fmt.Errorf("atomically restore preserved plugin %q: %w", item.Action.Directory, err)
	}
	restoredFingerprint, restoredFiles, restoredBytes, restoredErr := fingerprintPluginTree(item.Action.TargetPath, resourceLimits)
	removedFingerprint, removedFiles, removedBytes, removedErr := fingerprintPluginTree(item.Action.PreservePath, resourceLimits)
	if restoredErr != nil || restoredFingerprint != item.Action.ExistingFingerprint || restoredFiles != item.Action.ExistingFiles || restoredBytes != item.Action.ExistingBytes || removedErr != nil || removedFingerprint != item.NewFingerprint || removedFiles != item.NewFiles || removedBytes != item.NewBytes {
		failure := retainIncomplete(transaction, errors.Join(fmt.Errorf("plugin rollback exchange failed identity verification for %q", item.Action.Directory), restoredErr, removedErr))
		_ = transaction.Close()
		return failure
	}
	quarantinePath := filepath.Join(transaction.Path, "installed")
	if err := moveNoReplace(home, item.Action.PreservePath, quarantinePath, false); err != nil {
		failure := retainIncomplete(transaction, fmt.Errorf("quarantine verified replaced plugin %q: %w", item.Action.Directory, err))
		_ = transaction.Close()
		return failure
	}
	quarantinedFingerprint, quarantinedFiles, quarantinedBytes, verifyErr := fingerprintPluginTree(quarantinePath, resourceLimits)
	if verifyErr != nil || quarantinedFingerprint != item.NewFingerprint || quarantinedFiles != item.NewFiles || quarantinedBytes != item.NewBytes {
		restoreErr := moveNoReplace(home, quarantinePath, item.Action.PreservePath, false)
		failure := retainIncomplete(transaction, errors.Join(fmt.Errorf("quarantined plugin changed during rollback: %q", item.Action.Directory), verifyErr, restoreErr))
		_ = transaction.Close()
		return failure
	}
	if err := transaction.Close(); err != nil {
		return fmt.Errorf("clean verified plugin rollback transaction %q: %w", item.Action.Directory, err)
	}
	return nil
}

func exchangePluginPaths(home, firstPath, secondPath string) error {
	firstParent, err := openSecureParent(home, firstPath, false)
	if err != nil {
		return err
	}
	defer firstParent.root.Close()
	secondParent, err := openSecureParent(home, secondPath, false)
	if err != nil {
		return err
	}
	defer secondParent.root.Close()
	if err := exchangeRoots(firstParent.root, firstParent.name, secondParent.root, secondParent.name); err != nil {
		return err
	}
	if err := syncNamespaceRoots(firstParent.root, secondParent.root); err != nil {
		return &namespaceMutationError{err: err}
	}
	return nil
}
