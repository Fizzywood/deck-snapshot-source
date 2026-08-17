package restore

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

type recoveryPluginPackage struct {
	Archive string
	SHA256  string
}

func createRecoveryPluginPackages(ctx context.Context, directory string, plan Plan, recoveryManifest manifest.Manifest, staged map[string]snapshot.StagedFile, resourceLimits limits.Limits) (map[string]recoveryPluginPackage, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("plugin recovery package directory must be absolute")
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create plugin recovery package directory: %w", err)
	}
	entriesByPath := make(map[string]manifest.File, len(recoveryManifest.Files))
	for _, entry := range recoveryManifest.Files {
		entriesByPath[entry.LogicalPath] = entry
	}
	packages := make(map[string]recoveryPluginPackage)
	packageIndex := 0
	for _, action := range plan.PluginActions {
		if (action.Operation != "replace" && action.Operation != "remove") || action.Method != pluginMethodDeckyAPI {
			continue
		}
		prefix := "recovery/plugins/" + action.Directory + "/"
		logicalPaths := make([]string, 0, action.ExistingFiles)
		for logicalPath := range entriesByPath {
			if strings.HasPrefix(logicalPath, prefix) {
				logicalPaths = append(logicalPaths, logicalPath)
			}
		}
		sort.Strings(logicalPaths)
		if len(logicalPaths) != action.ExistingFiles {
			return nil, fmt.Errorf("plugin recovery inventory is incomplete for %q", action.Directory)
		}
		archivePath := filepath.Join(directory, fmt.Sprintf("plugin-%04d.zip", packageIndex))
		packageIndex++
		if err := writeRecoveryPluginZIP(ctx, archivePath, action.Directory, prefix, logicalPaths, entriesByPath, staged, resourceLimits); err != nil {
			return nil, err
		}
		checksum, info, err := hashRegularFile(archivePath, pluginstore.MaxPackageTotal)
		if err != nil || info.Size() < 1 || info.Size() > pluginstore.MaxPackageTotal {
			return nil, errors.Join(fmt.Errorf("validate plugin recovery archive for %q", action.Directory), err)
		}
		packages[action.Directory] = recoveryPluginPackage{Archive: archivePath, SHA256: checksum}
	}
	return packages, nil
}

func writeRecoveryPluginZIP(ctx context.Context, archivePath, directory, prefix string, logicalPaths []string, entries map[string]manifest.File, staged map[string]snapshot.StagedFile, resourceLimits limits.Limits) (resultErr error) {
	output, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = output.Close()
		if remove {
			_ = os.Remove(archivePath)
		}
	}()
	writer := zip.NewWriter(output)
	closedWriter := false
	defer func() {
		if !closedWriter {
			_ = writer.Close()
		}
	}()
	directories := map[string]struct{}{directory + "/": {}}
	for _, logicalPath := range logicalPaths {
		relative := strings.TrimPrefix(logicalPath, prefix)
		if relative == logicalPath || relative == "" {
			return errors.New("plugin recovery logical path is invalid")
		}
		archiveName := directory + "/" + relative
		for parent := filepath.ToSlash(filepath.Dir(archiveName)); parent != "." && parent != directory; parent = filepath.ToSlash(filepath.Dir(parent)) {
			directories[parent+"/"] = struct{}{}
		}
	}
	directoryNames := make([]string, 0, len(directories))
	for name := range directories {
		directoryNames = append(directoryNames, name)
	}
	sort.Strings(directoryNames)
	for _, name := range directoryNames {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(os.ModeDir | 0o755)
		if _, err := writer.CreateHeader(header); err != nil {
			return err
		}
	}
	for _, logicalPath := range logicalPaths {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := entries[logicalPath]
		payload, exists := staged[logicalPath]
		if !exists || payload.Size != entry.Size || payload.SHA256 != entry.SHA256 {
			return fmt.Errorf("staged plugin recovery payload is missing for %q", logicalPath)
		}
		relative := strings.TrimPrefix(logicalPath, prefix)
		archiveName := directory + "/" + relative
		if err := manifest.ValidateLogicalPath("plugin/"+archiveName, resourceLimits.MaxPathLength); err != nil {
			return err
		}
		header := &zip.FileHeader{Name: archiveName, Method: zip.Deflate}
		header.SetMode(os.FileMode(entry.Mode))
		destination, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}
		before, err := os.Lstat(payload.Path)
		if err != nil || !before.Mode().IsRegular() || isLinkOrReparsePoint(before) || before.Size() != entry.Size {
			return fmt.Errorf("plugin recovery payload is unsafe for %q", logicalPath)
		}
		source, err := os.Open(payload.Path)
		if err != nil {
			return err
		}
		opened, statErr := source.Stat()
		if statErr != nil || !os.SameFile(before, opened) {
			source.Close()
			return errors.New("plugin recovery payload changed while opening")
		}
		hash := sha256.New()
		written, copyErr := copyRestoreContext(ctx, io.MultiWriter(destination, hash), io.LimitReader(source, entry.Size))
		closeErr := source.Close()
		if copyErr != nil || closeErr != nil || written != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return errors.Join(fmt.Errorf("plugin recovery payload failed verification for %q", logicalPath), copyErr, closeErr)
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	closedWriter = true
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}
