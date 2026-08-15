package pluginstore

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/manifest"
)

const (
	MaxPackageDownload = int64(256 << 20)
	MaxPackageFiles    = 10_000
	MaxPackageTotal    = int64(512 << 20)
	MaxPackageFile     = int64(128 << 20)
	MaxPackageRatio    = int64(200)
	PackageHTTPTimeout = 2 * time.Minute
)

type RemoteBinary struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	SHA256Hash string `json:"sha256hash"`
}

type PackageMetadata struct {
	Name           string         `json:"name"`
	Author         string         `json:"author"`
	Version        string         `json:"version"`
	RemoteBinaries []RemoteBinary `json:"remote_binaries,omitempty"`
}

type PreparedPackage struct {
	Directory string          `json:"directory"`
	Root      string          `json:"root"`
	Archive   string          `json:"archive"`
	Metadata  PackageMetadata `json:"metadata"`
	Files     int             `json:"files"`
	Bytes     int64           `json:"bytes"`
}

type packageJSON struct {
	Version      string         `json:"version"`
	RemoteBinary []RemoteBinary `json:"remote_binary"`
}

type pluginJSON struct {
	Name   string `json:"name"`
	Author string `json:"author"`
}

// PreparePackage downloads and verifies one exact official store resolution,
// strictly extracts it into an empty private workspace and verifies any
// declared remote binaries. It never executes package content.
func PreparePackage(ctx context.Context, resolution Resolution, workspace string, client *http.Client) (PreparedPackage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if resolution.Status != "resolved" || resolution.Blocking || !validSHA256(resolution.SHA256) || !ValidArtifactURL(resolution.ArtifactURL) {
		return PreparedPackage{}, errors.New("plugin resolution is not eligible for download")
	}
	if !filepath.IsAbs(workspace) {
		return PreparedPackage{}, errors.New("plugin workspace must be absolute")
	}
	if err := manifest.ValidateLogicalPath("plugin/"+resolution.SnapshotDirectory, 1024); err != nil || strings.Contains(resolution.SnapshotDirectory, "/") {
		return PreparedPackage{}, errors.New("plugin directory identity is unsafe")
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return PreparedPackage{}, fmt.Errorf("inspect plugin workspace: %w", err)
	}
	if len(entries) != 0 {
		return PreparedPackage{}, errors.New("plugin workspace must be empty")
	}
	if err := os.Chmod(workspace, 0o700); err != nil {
		return PreparedPackage{}, err
	}
	if client == nil {
		client = &http.Client{Timeout: PackageHTTPTimeout}
	}
	archivePath := filepath.Join(workspace, "package.zip")
	if err := downloadVerified(ctx, client, resolution.ArtifactURL, resolution.SHA256, archivePath, MaxPackageDownload, 0o600); err != nil {
		return PreparedPackage{}, fmt.Errorf("download plugin package: %w", err)
	}
	prepared, err := extractVerifiedPackage(ctx, resolution, archivePath, workspace)
	if err != nil {
		_ = os.Remove(archivePath)
		return PreparedPackage{}, err
	}
	prepared.Archive = archivePath
	bundledRemoteBinaries, err := validateRemoteBinaryDestinations(ctx, prepared.Root, prepared.Metadata.RemoteBinaries)
	if err != nil {
		return PreparedPackage{}, err
	}
	for _, binary := range prepared.Metadata.RemoteBinaries {
		if err := ctx.Err(); err != nil {
			return PreparedPackage{}, err
		}
		if err := validateRemoteBinary(binary); err != nil {
			return PreparedPackage{}, err
		}
		if _, bundled := bundledRemoteBinaries[strings.ToLower(binary.Name)]; bundled {
			continue
		}
		binDirectory := filepath.Join(prepared.Root, "bin")
		if err := os.MkdirAll(binDirectory, 0o700); err != nil {
			return PreparedPackage{}, err
		}
		destination := filepath.Join(binDirectory, binary.Name)
		if err := downloadVerified(ctx, client, binary.URL, strings.ToLower(binary.SHA256Hash), destination, MaxPackageFile, 0o700); err != nil {
			return PreparedPackage{}, fmt.Errorf("download remote binary %q: %w", binary.Name, err)
		}
		prepared.Files++
		info, err := os.Lstat(destination)
		if err != nil {
			return PreparedPackage{}, err
		}
		prepared.Bytes += info.Size()
		if prepared.Bytes > MaxPackageTotal || prepared.Files > MaxPackageFiles {
			return PreparedPackage{}, errors.New("plugin package exceeded limits after remote binaries")
		}
	}
	return prepared, nil
}

func extractVerifiedPackage(ctx context.Context, resolution Resolution, archivePath, workspace string) (PreparedPackage, error) {
	archiveInfo, err := os.Lstat(archivePath)
	if err != nil || !archiveInfo.Mode().IsRegular() || archiveInfo.Mode()&os.ModeSymlink != 0 {
		return PreparedPackage{}, errors.New("plugin archive is not a regular file")
	}
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return PreparedPackage{}, fmt.Errorf("open plugin ZIP: %w", err)
	}
	defer reader.Close()
	if len(reader.File) == 0 || len(reader.File) > MaxPackageFiles {
		return PreparedPackage{}, errors.New("plugin ZIP entry count is outside limits")
	}
	seen := make(map[string]struct{}, len(reader.File))
	caseFolded := make(map[string]string, len(reader.File))
	var topLevel string
	var total int64
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return PreparedPackage{}, err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if name == "" || strings.Contains(entry.Name, `\`) {
			return PreparedPackage{}, fmt.Errorf("plugin ZIP contains an unsafe path %q", entry.Name)
		}
		if err := manifest.ValidateLogicalPath(name, 1024); err != nil {
			return PreparedPackage{}, fmt.Errorf("plugin ZIP path %q: %w", entry.Name, err)
		}
		if _, duplicate := seen[name]; duplicate {
			return PreparedPackage{}, fmt.Errorf("plugin ZIP contains duplicate path %q", name)
		}
		seen[name] = struct{}{}
		folded := strings.ToLower(name)
		if previous, collision := caseFolded[folded]; collision && previous != name {
			return PreparedPackage{}, fmt.Errorf("plugin ZIP contains a case-normalization collision: %q and %q", previous, name)
		}
		caseFolded[folded] = name
		first, _, _ := strings.Cut(name, "/")
		if topLevel == "" {
			topLevel = first
		} else if first != topLevel {
			return PreparedPackage{}, errors.New("plugin ZIP must contain exactly one top-level directory")
		}
		mode := entry.Mode()
		if entry.FileInfo().IsDir() {
			continue
		}
		if !mode.IsRegular() || mode&os.ModeSymlink != 0 {
			return PreparedPackage{}, fmt.Errorf("plugin ZIP entry is not a regular file: %q", entry.Name)
		}
		size := int64(entry.UncompressedSize64)
		compressed := int64(entry.CompressedSize64)
		if size < 0 || size > MaxPackageFile || size > MaxPackageTotal-total {
			return PreparedPackage{}, fmt.Errorf("plugin ZIP entry exceeds limits: %q", entry.Name)
		}
		if size > 0 && (compressed == 0 || (size-1)/compressed+1 > MaxPackageRatio) {
			return PreparedPackage{}, fmt.Errorf("plugin ZIP entry exceeds compression-ratio limit: %q", entry.Name)
		}
		total += size
	}
	if topLevel != resolution.SnapshotDirectory {
		return PreparedPackage{}, fmt.Errorf("plugin ZIP top-level directory %q does not match snapshot identity %q", topLevel, resolution.SnapshotDirectory)
	}
	if total > 0 && archiveInfo.Size() > 0 && (total-1)/archiveInfo.Size()+1 > MaxPackageRatio {
		return PreparedPackage{}, errors.New("plugin ZIP exceeds aggregate compression-ratio limit")
	}

	root, err := os.OpenRoot(workspace)
	if err != nil {
		return PreparedPackage{}, err
	}
	defer root.Close()
	files := 0
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return PreparedPackage{}, err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if entry.FileInfo().IsDir() {
			if err := root.MkdirAll(filepath.FromSlash(name), 0o700); err != nil {
				return PreparedPackage{}, fmt.Errorf("create plugin staging directory: %w", err)
			}
			continue
		}
		parent := filepath.Dir(filepath.FromSlash(name))
		if err := root.MkdirAll(parent, 0o700); err != nil {
			return PreparedPackage{}, err
		}
		input, err := entry.Open()
		if err != nil {
			return PreparedPackage{}, err
		}
		mode := os.FileMode(0o600)
		if entry.Mode().Perm()&0o111 != 0 {
			mode = 0o700
		}
		output, err := root.OpenFile(filepath.FromSlash(name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			input.Close()
			return PreparedPackage{}, err
		}
		written, copyErr := copyContext(ctx, output, io.LimitReader(input, int64(entry.UncompressedSize64)+1))
		closeInputErr := input.Close()
		syncErr := output.Sync()
		closeOutputErr := output.Close()
		if copyErr != nil || closeInputErr != nil || syncErr != nil || closeOutputErr != nil || written != int64(entry.UncompressedSize64) {
			return PreparedPackage{}, fmt.Errorf("extract plugin ZIP entry %q failed verification", entry.Name)
		}
		files++
	}

	packageRoot := filepath.Join(workspace, filepath.FromSlash(topLevel))
	metadata, err := readPackageMetadata(packageRoot)
	if err != nil {
		return PreparedPackage{}, err
	}
	if metadata.Name != resolution.StoreName || metadata.Author != resolution.StoreAuthor || metadata.Version != resolution.ResolvedVersion {
		return PreparedPackage{}, fmt.Errorf("plugin package identity mismatch: got %q by %q version %q", metadata.Name, metadata.Author, metadata.Version)
	}
	return PreparedPackage{Directory: topLevel, Root: packageRoot, Metadata: metadata, Files: files, Bytes: total}, nil
}

func readPackageMetadata(root string) (PackageMetadata, error) {
	pluginBytes, err := readBoundedRegular(filepath.Join(root, "plugin.json"), 1<<20)
	if err != nil {
		return PackageMetadata{}, fmt.Errorf("read plugin.json: %w", err)
	}
	packageBytes, err := readBoundedRegular(filepath.Join(root, "package.json"), 1<<20)
	if err != nil {
		return PackageMetadata{}, fmt.Errorf("read package.json: %w", err)
	}
	var plugin pluginJSON
	var packageValue packageJSON
	if err := json.Unmarshal(pluginBytes, &plugin); err != nil {
		return PackageMetadata{}, fmt.Errorf("decode plugin.json: %w", err)
	}
	if err := json.Unmarshal(packageBytes, &packageValue); err != nil {
		return PackageMetadata{}, fmt.Errorf("decode package.json: %w", err)
	}
	if plugin.Name == "" || plugin.Author == "" || packageValue.Version == "" {
		return PackageMetadata{}, errors.New("plugin package metadata is incomplete")
	}
	return PackageMetadata{Name: plugin.Name, Author: plugin.Author, Version: packageValue.Version, RemoteBinaries: packageValue.RemoteBinary}, nil
}

// InspectPackageMetadata reads the required identity files from an existing
// package directory without executing any package content.
func InspectPackageMetadata(root string) (PackageMetadata, error) {
	if !filepath.IsAbs(root) {
		return PackageMetadata{}, errors.New("plugin package root must be absolute")
	}
	return readPackageMetadata(root)
}

func readBoundedRegular(filePath string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximum {
		return nil, errors.New("metadata file is not a bounded regular file")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(contents)) != info.Size() {
		return nil, errors.New("metadata file changed while reading")
	}
	return contents, nil
}

func validateRemoteBinary(binary RemoteBinary) error {
	if binary.Name == "" || path.Base(binary.Name) != binary.Name || strings.ContainsAny(binary.Name, `/\`) || manifest.ValidateLogicalPath("bin/"+binary.Name, 1024) != nil || !validSHA256(binary.SHA256Hash) || !ValidArtifactURL(binary.URL) {
		return fmt.Errorf("plugin remote binary metadata is unsafe for %q", binary.Name)
	}
	return nil
}

func validateRemoteBinaryDestinations(ctx context.Context, root string, binaries []RemoteBinary) (map[string]struct{}, error) {
	seen := make(map[string]struct{}, len(binaries))
	for _, binary := range binaries {
		if err := validateRemoteBinary(binary); err != nil {
			return nil, err
		}
		folded := strings.ToLower(binary.Name)
		if _, duplicate := seen[folded]; duplicate {
			return nil, fmt.Errorf("plugin declares duplicate remote binary %q", binary.Name)
		}
		seen[folded] = struct{}{}
	}
	binDirectory := filepath.Join(root, "bin")
	entries, err := os.ReadDir(binDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, err
	}
	existing := make(map[string]string, len(entries))
	for _, entry := range entries {
		existing[strings.ToLower(entry.Name())] = entry.Name()
	}
	bundled := make(map[string]struct{})
	for _, binary := range binaries {
		folded := strings.ToLower(binary.Name)
		existingName, collision := existing[folded]
		if !collision {
			continue
		}
		if existingName != binary.Name {
			return nil, fmt.Errorf("plugin ZIP contains case-colliding remote-binary destination %q", binary.Name)
		}
		matched, matchErr := regularFileMatchesSHA256(ctx, filepath.Join(binDirectory, existingName), strings.ToLower(binary.SHA256Hash), MaxPackageFile)
		if matchErr != nil {
			return nil, fmt.Errorf("verify bundled remote binary %q: %w", binary.Name, matchErr)
		}
		if !matched {
			return nil, fmt.Errorf("plugin ZIP already contains mismatched remote-binary destination %q", binary.Name)
		}
		bundled[folded] = struct{}{}
	}
	return bundled, nil
}

func regularFileMatchesSHA256(ctx context.Context, filePath, expectedHash string, maximum int64) (bool, error) {
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maximum {
		return false, errors.New("bundled remote binary is not a bounded regular file")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return false, errors.New("bundled remote binary changed while opening")
	}
	digest := sha256.New()
	written, err := copyContext(ctx, digest, io.LimitReader(file, maximum+1))
	if err != nil || written != opened.Size() {
		return false, errors.New("bundled remote binary changed while hashing")
	}
	return hex.EncodeToString(digest.Sum(nil)) == expectedHash, nil
}

func downloadVerified(ctx context.Context, client *http.Client, sourceURL, expectedHash, destination string, maximum int64, mode os.FileMode) error {
	if !ValidArtifactURL(sourceURL) || !validSHA256(expectedHash) {
		return errors.New("download URL or checksum is invalid")
	}
	downloadContext, cancel := context.WithTimeout(ctx, PackageHTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(downloadContext, http.MethodGet, sourceURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "Deck-Snapshot/0.1")
	safeClient := *client
	if safeClient.Timeout <= 0 || safeClient.Timeout > PackageHTTPTimeout {
		safeClient.Timeout = PackageHTTPTimeout
	}
	originalRedirect := client.CheckRedirect
	safeClient.CheckRedirect = func(next *http.Request, previous []*http.Request) error {
		if next == nil || next.URL == nil || !validDownloadRedirectURL(next.URL.String()) {
			return errors.New("download redirect URL is unsafe")
		}
		if len(previous) >= 10 {
			return errors.New("download exceeded the redirect limit")
		}
		if originalRedirect != nil {
			return originalRedirect(next, previous)
		}
		return nil
	}
	response, err := safeClient.Do(request)
	if err != nil {
		if contextErr := downloadContext.Err(); contextErr != nil {
			return contextErr
		}
		return errors.New("download request failed before a verified response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", response.StatusCode)
	}
	if response.Request == nil || response.Request.URL == nil || !validDownloadRedirectURL(response.Request.URL.String()) {
		return errors.New("download redirected to an unsafe URL")
	}
	if response.ContentLength > maximum {
		return errors.New("download exceeds the size limit")
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		file.Close()
		if remove {
			_ = os.Remove(destination)
		}
	}()
	digest := sha256.New()
	written, copyErr := copyContext(downloadContext, io.MultiWriter(file, digest), io.LimitReader(response.Body, maximum+1))
	if copyErr != nil || written > maximum || hex.EncodeToString(digest.Sum(nil)) != strings.ToLower(expectedHash) {
		return errors.New("download failed size or checksum verification")
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func validDownloadRedirectURL(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if parsed.RawQuery == "" {
		return true
	}
	return strings.EqualFold(parsed.Hostname(), "release-assets.githubusercontent.com")
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
