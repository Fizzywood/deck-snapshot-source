package snapshot

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
)

// StagedFile is a verified snapshot payload copied to an opaque private path.
// Logical archive paths are never used as filesystem paths during staging.
type StagedFile struct {
	LogicalPath string
	Path        string
	Size        int64
	SHA256      string
}

// StageSelected validates an entire snapshot and copies only the selected
// payloads into a newly created private directory. The destination directory
// must already exist and must be empty.
func StageSelected(ctx context.Context, snapshotPath, directory string, logicalPaths []string, resourceLimits limits.Limits) (map[string]StagedFile, manifest.Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !filepath.IsAbs(snapshotPath) || !filepath.IsAbs(directory) {
		return nil, manifest.Manifest{}, errors.New("snapshot and staging paths must be absolute")
	}
	if err := resourceLimits.Validate(); err != nil {
		return nil, manifest.Manifest{}, err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, manifest.Manifest{}, fmt.Errorf("inspect staging directory: %w", err)
	}
	if len(entries) != 0 {
		return nil, manifest.Manifest{}, errors.New("staging directory must be empty")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, manifest.Manifest{}, fmt.Errorf("protect staging directory: %w", err)
	}

	value, err := ValidateContext(ctx, snapshotPath, resourceLimits)
	if err != nil {
		return nil, manifest.Manifest{}, err
	}
	requested := make(map[string]struct{}, len(logicalPaths))
	for _, logicalPath := range logicalPaths {
		if err := manifest.ValidateLogicalPath(logicalPath, resourceLimits.MaxPathLength); err != nil {
			return nil, manifest.Manifest{}, err
		}
		if _, exists := requested[logicalPath]; exists {
			return nil, manifest.Manifest{}, fmt.Errorf("duplicate staged path %q", logicalPath)
		}
		requested[logicalPath] = struct{}{}
	}
	expected := make(map[string]manifest.File, len(value.Files))
	for _, entry := range value.Files {
		expected[entry.LogicalPath] = entry
	}
	for logicalPath := range requested {
		if _, exists := expected[logicalPath]; !exists {
			return nil, manifest.Manifest{}, fmt.Errorf("selected path is not declared by the snapshot: %q", logicalPath)
		}
	}

	before, err := os.Lstat(snapshotPath)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return nil, manifest.Manifest{}, errors.New("snapshot identity is unsafe before staging")
	}
	file, err := os.Open(snapshotPath)
	if err != nil {
		return nil, manifest.Manifest{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, manifest.Manifest{}, errors.New("snapshot identity changed before staging")
	}

	buffered := bufio.NewReader(&contextReader{ctx: ctx, reader: file})
	compressed, err := gzip.NewReader(buffered)
	if err != nil {
		return nil, manifest.Manifest{}, err
	}
	compressed.Multistream(false)
	archive := tar.NewReader(compressed)
	staged := make(map[string]StagedFile, len(requested))
	createdPaths := make([]string, 0, len(requested))
	cleanup := func() {
		for _, path := range createdPaths {
			_ = os.Remove(path)
		}
	}
	seen := make(map[string]struct{}, len(expected))
	entryNumber := 0
	for {
		if err := ctx.Err(); err != nil {
			compressed.Close()
			cleanup()
			return nil, manifest.Manifest{}, err
		}
		header, nextErr := archive.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			compressed.Close()
			cleanup()
			return nil, manifest.Manifest{}, nextErr
		}
		entryNumber++
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			compressed.Close()
			cleanup()
			return nil, manifest.Manifest{}, fmt.Errorf("unsupported staged archive entry type for %q", header.Name)
		}
		if entryNumber == 1 {
			if header.Name != manifestPath || header.Size > resourceLimits.MaxManifestSize {
				compressed.Close()
				cleanup()
				return nil, manifest.Manifest{}, errors.New("snapshot manifest entry changed during staging")
			}
			if written, err := copyContext(ctx, io.Discard, io.LimitReader(archive, header.Size)); err != nil || written != header.Size {
				compressed.Close()
				cleanup()
				return nil, manifest.Manifest{}, err
			}
			continue
		}
		entry, exists := expected[header.Name]
		if !exists || header.Size != entry.Size {
			compressed.Close()
			cleanup()
			return nil, manifest.Manifest{}, fmt.Errorf("snapshot inventory changed during staging at %q", header.Name)
		}
		if _, duplicate := seen[header.Name]; duplicate {
			compressed.Close()
			cleanup()
			return nil, manifest.Manifest{}, fmt.Errorf("duplicate staged archive entry %q", header.Name)
		}
		seen[header.Name] = struct{}{}
		hash := sha256.New()
		writer := io.Writer(hash)
		var output *os.File
		var outputPath string
		if _, selected := requested[header.Name]; selected {
			outputPath = filepath.Join(directory, fmt.Sprintf("payload-%08d.bin", len(createdPaths)))
			output, err = os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				compressed.Close()
				cleanup()
				return nil, manifest.Manifest{}, err
			}
			createdPaths = append(createdPaths, outputPath)
			writer = io.MultiWriter(output, hash)
		}
		written, copyErr := copyContext(ctx, writer, io.LimitReader(archive, header.Size))
		if output != nil {
			if syncErr := output.Sync(); copyErr == nil && syncErr != nil {
				copyErr = syncErr
			}
			if closeErr := output.Close(); copyErr == nil && closeErr != nil {
				copyErr = closeErr
			}
		}
		actualHash := hex.EncodeToString(hash.Sum(nil))
		if copyErr != nil || written != header.Size || actualHash != entry.SHA256 {
			compressed.Close()
			cleanup()
			return nil, manifest.Manifest{}, fmt.Errorf("staged payload failed verification: %q", header.Name)
		}
		if outputPath != "" {
			staged[header.Name] = StagedFile{LogicalPath: header.Name, Path: outputPath, Size: entry.Size, SHA256: entry.SHA256}
		}
	}
	if len(seen) != len(expected) || len(staged) != len(requested) {
		compressed.Close()
		cleanup()
		return nil, manifest.Manifest{}, errors.New("snapshot inventory was incomplete during staging")
	}
	var decodedTrailer [1]byte
	if count, err := compressed.Read(decodedTrailer[:]); count != 0 {
		compressed.Close()
		cleanup()
		return nil, manifest.Manifest{}, errors.New("snapshot contains decoded data after the tar terminator during staging")
	} else if !errors.Is(err, io.EOF) {
		compressed.Close()
		cleanup()
		return nil, manifest.Manifest{}, err
	}
	if err := compressed.Close(); err != nil {
		cleanup()
		return nil, manifest.Manifest{}, err
	}
	if _, err := buffered.Peek(1); err == nil || !errors.Is(err, io.EOF) {
		cleanup()
		return nil, manifest.Manifest{}, errors.New("snapshot contains trailing data during staging")
	}
	after, err := os.Lstat(snapshotPath)
	if err != nil || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		cleanup()
		return nil, manifest.Manifest{}, errors.New("snapshot changed during staging")
	}

	return staged, value, nil
}
