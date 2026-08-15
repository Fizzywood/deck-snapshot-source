// Package snapshot writes and validates constrained immutable snapshot archives.
package snapshot

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/discovery"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
)

const manifestPath = "manifest.json"

type Created struct {
	Path     string            `json:"path"`
	Size     int64             `json:"size"`
	Manifest manifest.Manifest `json:"manifest"`
}

type Summary struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Valid      bool   `json:"valid"`
	SnapshotID string `json:"snapshot_id,omitempty"`
	CreatedUTC string `json:"created_utc,omitempty"`
	Error      string `json:"error,omitempty"`
}

func Create(ctx context.Context, directory string, result discovery.Result, resourceLimits limits.Limits) (Created, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !filepath.IsAbs(directory) {
		return Created{}, errors.New("snapshot directory must be absolute")
	}
	if err := resourceLimits.Validate(); err != nil {
		return Created{}, err
	}
	if err := result.Manifest.Validate(resourceLimits.MaxPathLength); err != nil {
		return Created{}, fmt.Errorf("validate manifest before creation: %w", err)
	}
	if err := validateManifestLimits(result.Manifest, resourceLimits); err != nil {
		return Created{}, err
	}
	if err := validateCandidates(result); err != nil {
		return Created{}, err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return Created{}, fmt.Errorf("create snapshot directory: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return Created{}, errors.New("snapshot directory is not a real directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return Created{}, fmt.Errorf("open snapshot directory: %w", err)
	}
	defer root.Close()
	createdTime, err := time.Parse(time.RFC3339Nano, result.Manifest.CreatedUTC)
	if err != nil {
		return Created{}, fmt.Errorf("parse manifest creation time: %w", err)
	}
	filename := fmt.Sprintf("deck-snapshot-%s-%s.tar.gz", createdTime.UTC().Format("20060102T150405Z"), result.Manifest.SnapshotID)
	finalPath := filepath.Join(directory, filename)
	finalName := filepath.Base(finalPath)
	if _, err := root.Lstat(finalName); err == nil {
		return Created{}, fmt.Errorf("snapshot already exists: %s", filename)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Created{}, fmt.Errorf("inspect final snapshot path: %w", err)
	}

	identifier := make([]byte, 12)
	if _, err := rand.Read(identifier); err != nil {
		return Created{}, err
	}
	temporaryName := ".deck-snapshot-" + hex.EncodeToString(identifier) + ".tmp"
	temporaryPath := filepath.Join(directory, temporaryName)
	temporary, err := root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Created{}, fmt.Errorf("create private snapshot temporary file: %w", err)
	}
	removeTemporary := true
	defer func() {
		temporary.Close()
		if removeTemporary {
			_ = root.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return Created{}, fmt.Errorf("protect snapshot temporary file: %w", err)
	}
	if err := writeArchive(ctx, temporary, result, resourceLimits, createdTime); err != nil {
		return Created{}, err
	}
	if err := temporary.Sync(); err != nil {
		return Created{}, fmt.Errorf("sync snapshot temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Created{}, fmt.Errorf("close snapshot temporary file: %w", err)
	}
	validated, err := ValidateContext(ctx, temporaryPath, resourceLimits)
	if err != nil {
		return Created{}, fmt.Errorf("validate completed snapshot: %w", err)
	}
	if validated.SnapshotID != result.Manifest.SnapshotID {
		return Created{}, errors.New("validated snapshot identity does not match creation request")
	}
	if err := publishNoReplace(root, temporaryName, finalName); err != nil {
		return Created{}, fmt.Errorf("publish snapshot atomically: %w", err)
	}
	removeTemporary = false
	if err := syncPublishedDirectory(root); err != nil {
		return Created{}, fmt.Errorf("durably publish snapshot: %w", err)
	}
	info, err := root.Stat(finalName)
	if err != nil {
		return Created{}, fmt.Errorf("inspect published snapshot: %w", err)
	}
	return Created{Path: finalPath, Size: info.Size(), Manifest: validated}, nil
}

func writeArchive(ctx context.Context, output io.Writer, result discovery.Result, resourceLimits limits.Limits, created time.Time) error {
	manifestBytes, err := json.MarshalIndent(result.Manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if int64(len(manifestBytes)) > resourceLimits.MaxManifestSize {
		return errors.New("manifest exceeds the configured size limit")
	}

	compressed, err := gzip.NewWriterLevel(output, gzip.BestSpeed)
	if err != nil {
		return fmt.Errorf("create gzip writer: %w", err)
	}
	compressed.Header.ModTime = created.UTC()
	compressed.Header.OS = 255
	archive := tar.NewWriter(compressed)
	closeWithError := func(current error) error {
		if err := archive.Close(); current == nil && err != nil {
			current = fmt.Errorf("close tar archive: %w", err)
		}
		if err := compressed.Close(); current == nil && err != nil {
			current = fmt.Errorf("close gzip stream: %w", err)
		}
		return current
	}

	if err := writeBytes(archive, manifestPath, manifestBytes, created); err != nil {
		return closeWithError(err)
	}
	candidates := append([]discovery.Candidate(nil), result.Candidates...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Entry.LogicalPath < candidates[j].Entry.LogicalPath })
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return closeWithError(err)
		}
		if err := writeCandidate(ctx, archive, candidate, created); err != nil {
			return closeWithError(err)
		}
	}
	return closeWithError(nil)
}

func writeBytes(archive *tar.Writer, name string, data []byte, created time.Time) error {
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: created.UTC(), Format: tar.FormatPAX}
	if err := archive.WriteHeader(header); err != nil {
		return fmt.Errorf("write %s header: %w", name, err)
	}
	if _, err := archive.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func writeCandidate(ctx context.Context, archive *tar.Writer, candidate discovery.Candidate, created time.Time) error {
	header := &tar.Header{Name: candidate.Entry.LogicalPath, Mode: 0o600, Size: candidate.Entry.Size, ModTime: created.UTC(), Format: tar.FormatPAX}
	if err := archive.WriteHeader(header); err != nil {
		return fmt.Errorf("write %s header: %w", candidate.Entry.LogicalPath, err)
	}
	if candidate.Entry.Generated {
		if int64(len(candidate.Data)) != candidate.Entry.Size || hashBytes(candidate.Data) != candidate.Entry.SHA256 {
			return fmt.Errorf("generated content changed for %s", candidate.Entry.LogicalPath)
		}
		if _, err := archive.Write(candidate.Data); err != nil {
			return fmt.Errorf("write %s: %w", candidate.Entry.LogicalPath, err)
		}
		return nil
	}
	return copyVerifiedSource(ctx, archive, candidate)
}

func copyVerifiedSource(ctx context.Context, destination io.Writer, candidate discovery.Candidate) error {
	before, err := os.Lstat(candidate.SourcePath)
	if err != nil {
		return fmt.Errorf("inspect source %s: %w", candidate.Entry.LogicalPath, err)
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source is no longer a regular file: %s", candidate.Entry.LogicalPath)
	}
	file, err := os.Open(candidate.SourcePath)
	if err != nil {
		return fmt.Errorf("open source %s: %w", candidate.Entry.LogicalPath, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() != candidate.Entry.Size {
		return fmt.Errorf("source identity changed: %s", candidate.Entry.LogicalPath)
	}
	hash := sha256.New()
	written, err := copyContext(ctx, io.MultiWriter(destination, hash), io.LimitReader(file, candidate.Entry.Size))
	if err != nil {
		return fmt.Errorf("copy source %s: %w", candidate.Entry.LogicalPath, err)
	}
	if written != candidate.Entry.Size {
		return fmt.Errorf("source was truncated: %s", candidate.Entry.LogicalPath)
	}
	var extra [1]byte
	if count, err := file.Read(extra[:]); err != nil && !errors.Is(err, io.EOF) {
		return err
	} else if count != 0 {
		return fmt.Errorf("source grew during creation: %s", candidate.Entry.LogicalPath)
	}
	after, err := os.Lstat(candidate.SourcePath)
	if err != nil {
		return err
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return fmt.Errorf("source changed during creation: %s", candidate.Entry.LogicalPath)
	}
	if hex.EncodeToString(hash.Sum(nil)) != candidate.Entry.SHA256 {
		return fmt.Errorf("source checksum changed: %s", candidate.Entry.LogicalPath)
	}
	return nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func validateCandidates(result discovery.Result) error {
	expected := make(map[string]manifest.File, len(result.Manifest.Files))
	for _, entry := range result.Manifest.Files {
		expected[entry.LogicalPath] = entry
	}
	seen := make(map[string]struct{}, len(result.Candidates))
	for _, candidate := range result.Candidates {
		entry, ok := expected[candidate.Entry.LogicalPath]
		if !ok || entry != candidate.Entry {
			return fmt.Errorf("candidate does not match manifest: %s", candidate.Entry.LogicalPath)
		}
		if _, exists := seen[candidate.Entry.LogicalPath]; exists {
			return fmt.Errorf("duplicate candidate: %s", candidate.Entry.LogicalPath)
		}
		seen[candidate.Entry.LogicalPath] = struct{}{}
		if candidate.Entry.Generated && candidate.SourcePath != "" {
			return fmt.Errorf("generated candidate has a source path: %s", candidate.Entry.LogicalPath)
		}
		if !candidate.Entry.Generated && candidate.SourcePath == "" {
			return fmt.Errorf("source candidate has no source path: %s", candidate.Entry.LogicalPath)
		}
	}
	if len(seen) != len(expected) {
		return errors.New("manifest and candidate counts differ")
	}
	return nil
}

func hashBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func Validate(snapshotPath string, resourceLimits limits.Limits) (manifest.Manifest, error) {
	return ValidateContext(context.Background(), snapshotPath, resourceLimits)
}

// ValidateContext validates a snapshot with bounded work and cancellation.
func ValidateContext(ctx context.Context, snapshotPath string, resourceLimits limits.Limits) (manifest.Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := resourceLimits.Validate(); err != nil {
		return manifest.Manifest{}, err
	}
	info, err := os.Lstat(snapshotPath)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("inspect snapshot: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return manifest.Manifest{}, errors.New("snapshot must be a regular file, not a link")
	}
	if info.Size() <= 0 || info.Size() > resourceLimits.MaxTotalSize+resourceLimits.MaxManifestSize {
		return manifest.Manifest{}, errors.New("snapshot compressed size is outside configured limits")
	}
	file, err := os.Open(snapshotPath)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("open snapshot: %w", err)
	}
	defer file.Close()
	buffered := bufio.NewReader(&contextReader{ctx: ctx, reader: file})
	compressed, err := gzip.NewReader(buffered)
	if err != nil {
		return manifest.Manifest{}, fmt.Errorf("open gzip stream: %w", err)
	}
	compressed.Multistream(false)
	archive := tar.NewReader(compressed)

	var value manifest.Manifest
	var expected map[string]manifest.File
	seen := make(map[string]struct{})
	counter := limits.Counter{Limits: resourceLimits}
	var totalUncompressed int64
	entryNumber := 0
	for {
		if err := ctx.Err(); err != nil {
			compressed.Close()
			return manifest.Manifest{}, err
		}
		header, nextErr := archive.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			compressed.Close()
			return manifest.Manifest{}, fmt.Errorf("read tar entry: %w", nextErr)
		}
		entryNumber++
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			compressed.Close()
			return manifest.Manifest{}, fmt.Errorf("unsupported tar entry type for %q", header.Name)
		}
		if header.Size < 0 {
			compressed.Close()
			return manifest.Manifest{}, fmt.Errorf("negative size for %q", header.Name)
		}
		if entryNumber == 1 {
			if header.Name != manifestPath {
				compressed.Close()
				return manifest.Manifest{}, errors.New("manifest.json must be the first archive entry")
			}
			if header.Size > resourceLimits.MaxManifestSize {
				compressed.Close()
				return manifest.Manifest{}, errors.New("manifest exceeds configured size limit")
			}
			contents, readErr := readExactEntryContext(ctx, archive, header.Size)
			if readErr != nil {
				compressed.Close()
				return manifest.Manifest{}, fmt.Errorf("read manifest: %w", readErr)
			}
			decoder := json.NewDecoder(bytes.NewReader(contents))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&value); err != nil {
				compressed.Close()
				return manifest.Manifest{}, fmt.Errorf("decode manifest: %w", err)
			}
			if err := value.Validate(resourceLimits.MaxPathLength); err != nil {
				compressed.Close()
				return manifest.Manifest{}, err
			}
			if err := validateManifestLimits(value, resourceLimits); err != nil {
				compressed.Close()
				return manifest.Manifest{}, err
			}
			expected = make(map[string]manifest.File, len(value.Files))
			for _, entry := range value.Files {
				expected[entry.LogicalPath] = entry
			}
			totalUncompressed += header.Size
			continue
		}
		if err := manifest.ValidateLogicalPath(header.Name, resourceLimits.MaxPathLength); err != nil {
			compressed.Close()
			return manifest.Manifest{}, fmt.Errorf("unsafe archive path %q: %w", header.Name, err)
		}
		if _, exists := seen[header.Name]; exists {
			compressed.Close()
			return manifest.Manifest{}, fmt.Errorf("duplicate archive entry %q", header.Name)
		}
		entry, ok := expected[header.Name]
		if !ok {
			compressed.Close()
			return manifest.Manifest{}, fmt.Errorf("undeclared archive entry %q", header.Name)
		}
		if header.Size != entry.Size {
			compressed.Close()
			return manifest.Manifest{}, fmt.Errorf("size mismatch for %q", header.Name)
		}
		if err := counter.Add(header.Size); err != nil {
			compressed.Close()
			return manifest.Manifest{}, fmt.Errorf("archive limits for %q: %w", header.Name, err)
		}
		hash := sha256.New()
		written, copyErr := copyContext(ctx, hash, io.LimitReader(archive, header.Size))
		if copyErr != nil || written != header.Size {
			compressed.Close()
			return manifest.Manifest{}, fmt.Errorf("truncated archive entry %q", header.Name)
		}
		if hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			compressed.Close()
			return manifest.Manifest{}, fmt.Errorf("checksum mismatch for %q", header.Name)
		}
		seen[header.Name] = struct{}{}
		totalUncompressed += header.Size
	}
	if entryNumber == 0 {
		compressed.Close()
		return manifest.Manifest{}, errors.New("snapshot archive is empty")
	}
	if len(seen) != len(expected) {
		compressed.Close()
		return manifest.Manifest{}, errors.New("archive is missing one or more declared files")
	}
	var decodedTrailer [1]byte
	if count, err := compressed.Read(decodedTrailer[:]); count != 0 {
		compressed.Close()
		return manifest.Manifest{}, errors.New("snapshot contains decoded data after the tar terminator")
	} else if !errors.Is(err, io.EOF) {
		compressed.Close()
		return manifest.Manifest{}, fmt.Errorf("verify gzip checksum: %w", err)
	}
	if err := compressed.Close(); err != nil {
		return manifest.Manifest{}, fmt.Errorf("close gzip stream: %w", err)
	}
	if _, err := buffered.Peek(1); err == nil {
		return manifest.Manifest{}, errors.New("snapshot contains trailing data or an extra gzip member")
	} else if !errors.Is(err, io.EOF) {
		return manifest.Manifest{}, fmt.Errorf("inspect snapshot trailer: %w", err)
	}
	if info.Size() > 0 && totalUncompressed > 0 && (totalUncompressed-1)/info.Size()+1 > resourceLimits.MaxCompressionRatio {
		return manifest.Manifest{}, errors.New("snapshot exceeds the configured compression ratio")
	}
	return value, nil
}

func readExactEntry(reader io.Reader, size int64) ([]byte, error) {
	contents := make([]byte, size)
	if _, err := io.ReadFull(reader, contents); err != nil {
		return nil, err
	}
	return contents, nil
}

func readExactEntryContext(ctx context.Context, reader io.Reader, size int64) ([]byte, error) {
	var contents bytes.Buffer
	contents.Grow(int(size))
	written, err := copyContext(ctx, &contents, io.LimitReader(reader, size))
	if err != nil {
		return nil, err
	}
	if written != size {
		return nil, io.ErrUnexpectedEOF
	}
	return contents.Bytes(), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(destination []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(destination)
}

func validateManifestLimits(value manifest.Manifest, resourceLimits limits.Limits) error {
	if len(value.Files) > resourceLimits.MaxFiles {
		return fmt.Errorf("manifest file count exceeds %d entries", resourceLimits.MaxFiles)
	}
	collections := []struct {
		name  string
		count int
	}{
		{name: "Steam account", count: len(value.Accounts)},
		{name: "plugin", count: len(value.Plugins)},
		{name: "CSS theme", count: len(value.CSSThemes)},
		{name: "artwork", count: len(value.Artwork)},
		{name: "exclusion", count: len(value.Exclusions)},
		{name: "warning", count: len(value.Warnings)},
		{name: "compatibility behavior", count: len(value.Compatibility.UnverifiedBehaviors)},
	}
	for _, collection := range collections {
		if collection.count > resourceLimits.MaxFiles {
			return fmt.Errorf("manifest %s count exceeds %d entries", collection.name, resourceLimits.MaxFiles)
		}
	}
	var total int64
	for _, file := range value.Files {
		if file.Size > resourceLimits.MaxFileSize {
			return fmt.Errorf("manifest file %q exceeds the per-file size limit", file.LogicalPath)
		}
		if file.Size > resourceLimits.MaxTotalSize-total {
			return errors.New("manifest file sizes exceed the total-size limit")
		}
		total += file.Size
	}
	return nil
}

func List(directory string, resourceLimits limits.Limits) ([]Summary, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []Summary{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read snapshot directory: %w", err)
	}
	result := make([]Summary, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "deck-snapshot-") || !strings.HasSuffix(entry.Name(), ".tar.gz") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		summary := Summary{Name: entry.Name(), Path: path, Size: info.Size()}
		value, validateErr := Validate(path, resourceLimits)
		if validateErr != nil {
			summary.Error = validateErr.Error()
		} else {
			summary.Valid = true
			summary.SnapshotID = value.SnapshotID
			summary.CreatedUTC = value.CreatedUTC
		}
		result = append(result, summary)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name > result[j].Name })
	return result, nil
}
