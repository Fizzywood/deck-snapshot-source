// Package discovery classifies supported Decky, CSS Loader and Steam artwork state.
package discovery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
)

const secretScanLimit = 1 << 20

var sensitiveValuePattern = regexp.MustCompile(`(?i)["']?(access[_-]?token|refresh[_-]?token|api[_-]?key|client[_-]?secret|password|passphrase|authorization|recovery[_-]?key|token|secret|credential|cookie)["']?\s*[:=]\s*["']?[^\s,"'}]+`)

type Options struct {
	Paths         platform.Paths
	AppVersion    string
	DeviceID      string
	DeviceName    string
	SnapshotID    string
	OSReleasePath string
	Now           time.Time
	Limits        limits.Limits
}

type Candidate struct {
	SourcePath string
	Data       []byte
	Entry      manifest.File
}

type Result struct {
	Manifest   manifest.Manifest
	Candidates []Candidate
}

type Report struct {
	PluginCount  int   `json:"plugin_count"`
	ThemeCount   int   `json:"theme_count"`
	AccountCount int   `json:"account_count"`
	ArtworkCount int   `json:"artwork_count"`
	PayloadFiles int   `json:"payload_files"`
	PayloadBytes int64 `json:"payload_bytes"`
}

type builder struct {
	context    context.Context
	manifest   manifest.Manifest
	candidates []Candidate
	seen       map[string]struct{}
	warnings   map[string]struct{}
	counter    limits.Counter
}

func Discover(ctx context.Context, options Options) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := options.Limits.Validate(); err != nil {
		return Result{}, fmt.Errorf("validate discovery limits: %w", err)
	}
	if options.AppVersion == "" || options.DeviceID == "" {
		return Result{}, errors.New("app version and device id are required")
	}
	if options.Now.IsZero() {
		options.Now = time.Now()
	}
	if options.SnapshotID == "" {
		id, err := randomSnapshotID()
		if err != nil {
			return Result{}, err
		}
		options.SnapshotID = id
	}

	state := &builder{
		context:  ctx,
		manifest: manifest.New(options.SnapshotID, options.AppVersion, options.DeviceID, options.DeviceName, options.Now),
		seen:     make(map[string]struct{}),
		warnings: make(map[string]struct{}),
		counter:  limits.Counter{Limits: options.Limits},
	}
	state.manifest.Detected.SteamOSVersion = detectSteamOSVersion(options.OSReleasePath)
	state.manifest.Detected.DeckyVersion = detectDeckyVersion(options.Paths.Decky)

	if err := state.discoverDecky(options.Paths.Decky); err != nil {
		return Result{}, err
	}
	if err := state.discoverCSS(options.Paths.Decky); err != nil {
		return Result{}, err
	}
	if err := state.discoverSteam(options.Paths.Steam); err != nil {
		return Result{}, err
	}

	state.manifest.Normalize()
	report := Report{
		PluginCount:  len(state.manifest.Plugins),
		ThemeCount:   len(state.manifest.CSSThemes),
		AccountCount: len(state.manifest.Accounts),
		ArtworkCount: len(state.manifest.Artwork),
		PayloadFiles: state.counter.Files,
		PayloadBytes: state.counter.Bytes,
	}
	discoveryReport, err := marshalReport(report)
	if err != nil {
		return Result{}, err
	}
	warningsReport, err := marshalReport(state.manifest.Warnings)
	if err != nil {
		return Result{}, err
	}
	if err := state.addGenerated("reports/discovery.json", "reports", discoveryReport); err != nil {
		return Result{}, err
	}
	if err := state.addGenerated("reports/warnings.json", "reports", warningsReport); err != nil {
		return Result{}, err
	}

	state.manifest.Normalize()
	if err := state.manifest.Validate(options.Limits.MaxPathLength); err != nil {
		return Result{}, fmt.Errorf("validate discovered manifest: %w", err)
	}
	sort.Slice(state.candidates, func(i, j int) bool {
		return state.candidates[i].Entry.LogicalPath < state.candidates[j].Entry.LogicalPath
	})
	return Result{Manifest: state.manifest, Candidates: state.candidates}, nil
}

func randomSnapshotID() (string, error) {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate snapshot id: %w", err)
	}
	return "dsnap-" + hex.EncodeToString(value), nil
}

func marshalReport(value any) ([]byte, error) {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode discovery report: %w", err)
	}
	return append(contents, '\n'), nil
}

func (b *builder) addGenerated(logicalPath, component string, data []byte) error {
	entry := manifest.File{
		LogicalPath: logicalPath,
		Component:   component,
		Size:        int64(len(data)),
		SHA256:      hashBytes(data),
		Mode:        0o600,
		Generated:   true,
	}
	if err := b.addCandidate(Candidate{Data: data, Entry: entry}); err != nil {
		return err
	}
	return nil
}

func (b *builder) addSource(sourcePath, logicalPath, component string) (manifest.File, bool, error) {
	if err := b.context.Err(); err != nil {
		return manifest.File{}, false, err
	}
	if err := manifest.ValidateLogicalPath(logicalPath, b.counter.Limits.MaxPathLength); err != nil {
		return manifest.File{}, false, fmt.Errorf("logical path %q: %w", logicalPath, err)
	}
	if isSecretName(filepath.Base(sourcePath)) {
		b.exclude(logicalPath, component, "secret_like_filename")
		b.warn("secret_file_excluded", component, "A secret-like file was excluded from the snapshot: "+logicalPath)
		return manifest.File{}, false, nil
	}

	info, err := os.Lstat(sourcePath)
	if err != nil {
		return manifest.File{}, false, fmt.Errorf("inspect %s: %w", logicalPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		b.exclude(logicalPath, component, "symlink_not_followed")
		b.warn("symlink_excluded", component, "A symbolic link was not followed: "+logicalPath)
		return manifest.File{}, false, nil
	}
	if !info.Mode().IsRegular() {
		b.exclude(logicalPath, component, "special_file_not_supported")
		b.warn("special_file_excluded", component, "A non-regular file was excluded: "+logicalPath)
		return manifest.File{}, false, nil
	}
	if info.Size() > b.counter.Limits.MaxFileSize {
		b.exclude(logicalPath, component, "file_size_limit")
		b.warn("oversized_file_excluded", component, "A file exceeded the per-file snapshot limit: "+logicalPath)
		return manifest.File{}, false, nil
	}
	secret, err := containsSensitiveValue(sourcePath, info.Size())
	if err != nil {
		return manifest.File{}, false, fmt.Errorf("scan %s for secrets: %w", logicalPath, err)
	}
	if secret {
		b.exclude(logicalPath, component, "suspected_secret_content")
		b.warn("secret_content_excluded", component, "A file with a suspected credential value was excluded: "+logicalPath)
		return manifest.File{}, false, nil
	}

	hash, verifiedInfo, err := hashRegularFile(sourcePath, info)
	if err != nil {
		return manifest.File{}, false, fmt.Errorf("hash %s: %w", logicalPath, err)
	}
	entry := manifest.File{
		LogicalPath: logicalPath,
		Component:   component,
		Size:        verifiedInfo.Size(),
		SHA256:      hash,
		Mode:        uint32(verifiedInfo.Mode().Perm()),
	}
	if err := b.addCandidate(Candidate{SourcePath: sourcePath, Entry: entry}); err != nil {
		return manifest.File{}, false, err
	}
	return entry, true, nil
}

func (b *builder) addCandidate(candidate Candidate) error {
	if _, exists := b.seen[candidate.Entry.LogicalPath]; exists {
		return fmt.Errorf("duplicate logical path %q", candidate.Entry.LogicalPath)
	}
	if err := b.counter.Add(candidate.Entry.Size); err != nil {
		return fmt.Errorf("add %s: %w", candidate.Entry.LogicalPath, err)
	}
	b.seen[candidate.Entry.LogicalPath] = struct{}{}
	b.candidates = append(b.candidates, candidate)
	b.manifest.Files = append(b.manifest.Files, candidate.Entry)
	return nil
}

func (b *builder) addTree(root, logicalRoot, component string) (int, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("inspect %s root: %w", component, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0, fmt.Errorf("%s root is not a directory", component)
	}
	before := len(b.candidates)
	err = filepath.WalkDir(root, func(sourcePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := b.context.Err(); err != nil {
			return err
		}
		if sourcePath == root {
			return nil
		}
		if entry.IsDir() && strings.HasPrefix(entry.Name(), ".deck-snapshot-") {
			return filepath.SkipDir
		}
		relative, err := filepath.Rel(root, sourcePath)
		if err != nil {
			return err
		}
		logicalPath := path.Join(logicalRoot, filepath.ToSlash(relative))
		if entry.Type()&os.ModeSymlink != 0 {
			b.exclude(logicalPath, component, "symlink_not_followed")
			b.warn("symlink_excluded", component, "A symbolic link was not followed: "+logicalPath)
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		_, _, err = b.addSource(sourcePath, logicalPath, component)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("walk %s: %w", component, err)
	}
	return len(b.candidates) - before, nil
}

func (b *builder) exclude(logicalPath, component, reason string) {
	b.manifest.Exclusions = append(b.manifest.Exclusions, manifest.Exclusion{LogicalPath: logicalPath, Component: component, Reason: reason})
}

func (b *builder) warn(code, component, message string) {
	key := code + "\x00" + component + "\x00" + message
	if _, exists := b.warnings[key]; exists {
		return
	}
	b.warnings[key] = struct{}{}
	b.manifest.Warnings = append(b.manifest.Warnings, manifest.Warning{Code: code, Component: component, Message: message})
}

func hashBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func hashRegularFile(filePath string, before os.FileInfo) (string, os.FileInfo, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return "", nil, errors.New("file identity changed during discovery")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, opened.Size()+1))
	if err != nil {
		return "", nil, err
	}
	if written != opened.Size() {
		return "", nil, errors.New("file size changed during discovery")
	}
	after, err := os.Lstat(filePath)
	if err != nil {
		return "", nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return "", nil, errors.New("file changed during discovery")
	}
	return hex.EncodeToString(hash.Sum(nil)), opened, nil
}

func containsSensitiveValue(filePath string, size int64) (bool, error) {
	extension := strings.ToLower(filepath.Ext(filePath))
	switch extension {
	case ".json", ".conf", ".ini", ".yaml", ".yml", ".toml", ".txt":
	default:
		if !strings.EqualFold(filepath.Base(filePath), "STORE") {
			return false, nil
		}
	}
	if size > secretScanLimit {
		return true, nil
	}
	contents, err := os.ReadFile(filePath)
	if err != nil {
		return false, err
	}
	if !utf8.Valid(contents) {
		return false, nil
	}
	return sensitiveValuePattern.Match(contents), nil
}

func isSecretName(name string) bool {
	lower := strings.ToLower(name)
	if lower == ".env" || strings.HasPrefix(lower, ".env.") || lower == "rclone.conf" {
		return true
	}
	extension := strings.ToLower(filepath.Ext(lower))
	if extension == ".pem" || extension == ".key" || extension == ".p12" || extension == ".pfx" {
		return true
	}
	return strings.Contains(lower, "client_secret") || strings.Contains(lower, "recovery-key") || strings.Contains(lower, "recovery_key") || strings.Contains(lower, "credentials")
}

func ensureRealDirectory(directoryPath string) (bool, error) {
	info, err := os.Lstat(directoryPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, errors.New("path is not a real directory")
	}
	return true, nil
}

func readJSON(filePath string, value any) error {
	info, err := os.Lstat(filePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("JSON source is not a regular file")
	}
	if info.Size() > secretScanLimit {
		return errors.New("JSON source exceeds the metadata size limit")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, secretScanLimit+1))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
