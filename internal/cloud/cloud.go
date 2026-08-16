package cloud

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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

const (
	acknowledgementSchema = 1
	expectedRcloneVersion = "rclone v1.74.4"
	GoogleDriveBasePath   = "Deck Snapshot/Snapshots"
)

var snapshotNamePattern = regexp.MustCompile(`^deck-snapshot-[0-9]{8}T[0-9]{6}Z-[A-Za-z0-9._-]+\.tar\.gz$`)

type Manager struct {
	Runner                Runner
	SnapshotDirectory     string
	StateDirectory        string
	ConfigPath            string
	RecoveryPath          string
	ConfigPassword        string
	CryptRemote           string
	BaseRemote            string
	BasePath              string
	ExpectedBaseType      string
	ProtectionFingerprint string
	CryptPassword         string
	CryptPassword2        string
	RecoveryMaterial      *RecoveryMaterial
	ManualRecovery        bool
	AllowUnencryptedTest  bool
	Limits                limits.Limits
	googleOAuth           googleOAuthDependencies
}

type Status struct {
	Configured           bool   `json:"configured"`
	Protected            bool   `json:"protected"`
	RecoveryAcknowledged bool   `json:"recovery_acknowledged"`
	Remote               string `json:"remote"`
	Scope                string `json:"scope,omitempty"`
	OAuthScopes          string `json:"oauth_scopes,omitempty"`
	Folder               string `json:"folder,omitempty"`
	Legacy               bool   `json:"legacy"`
	ConfigurationMessage string `json:"configuration_message,omitempty"`
}

type Item struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime string `json:"modified_utc,omitempty"`
}

type acknowledgement struct {
	Schema      int    `json:"schema"`
	Fingerprint string `json:"fingerprint"`
	CreatedUTC  string `json:"created_utc"`
}

func (manager Manager) Check(ctx context.Context) (Status, error) {
	status, err := manager.InspectConfiguration(ctx)
	if err != nil {
		return status, err
	}
	if _, err := manager.listRemote(ctx); err != nil {
		message := fmt.Sprintf("contact protected cloud storage: %v", err)
		status.Configured = false
		status.ConfigurationMessage = message
		return status, errors.New(message)
	}
	return status, nil
}

// InspectConfiguration validates local encrypted cloud state without requiring
// provider reachability. It exists so a revoked or offline connection can still
// be forgotten locally without weakening the normal live status check.
func (manager Manager) InspectConfiguration(ctx context.Context) (Status, error) {
	if err := manager.validate(); err != nil {
		return Status{}, err
	}
	profile, err := manager.preflightProfile(ctx)
	if err != nil {
		return Status{Remote: manager.CryptRemote, ConfigurationMessage: err.Error()}, err
	}
	acknowledged, err := manager.recoveryAcknowledged()
	if err != nil {
		return Status{}, err
	}
	// The encrypted rclone configuration intentionally does not expose the
	// token's granted-scope claim through the redacted remote view. Do not
	// infer the new appData permission from the visible drive.file profile;
	// only a completed PKCE connection can report the exact two-scope set.
	return Status{Configured: true, Protected: true, RecoveryAcknowledged: acknowledged, Remote: manager.CryptRemote, Scope: profile.scope, Folder: profile.folder, Legacy: profile.legacy}, nil
}

func (manager Manager) AcknowledgeRecovery(now time.Time) error {
	if err := manager.validate(); err != nil {
		return err
	}
	if !validFingerprint(manager.ProtectionFingerprint) {
		return errors.New("cloud protection fingerprint is not a lowercase SHA-256 value")
	}
	acknowledged, err := manager.recoveryAcknowledged()
	if err == nil && acknowledged {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect recovery acknowledgement: %w", err)
	}
	directory := filepath.Join(manager.StateDirectory, "cloud")
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}
	value := acknowledgement{Schema: acknowledgementSchema, Fingerprint: manager.ProtectionFingerprint, CreatedUTC: now.UTC().Format(time.RFC3339Nano)}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return createPrivateFile(filepath.Join(directory, "recovery-acknowledgement.json"), encoded)
}

func (manager Manager) List(ctx context.Context) ([]Item, error) {
	if err := manager.preflight(ctx); err != nil {
		return nil, err
	}
	return manager.listRemote(ctx)
}

func (manager Manager) listRemote(ctx context.Context) ([]Item, error) {
	result, err := manager.run(ctx, "lsjson", manager.remoteRoot(), "--files-only", "--max-depth", "1")
	if err != nil {
		return nil, err
	}
	var values []struct {
		Name    string `json:"Name"`
		Size    int64  `json:"Size"`
		ModTime string `json:"ModTime"`
		IsDir   bool   `json:"IsDir"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &values); err != nil {
		return nil, fmt.Errorf("decode protected cloud listing: %w", err)
	}
	items := make([]Item, 0, len(values))
	for _, value := range values {
		if value.IsDir || !validSnapshotName(value.Name) || value.Size < 0 || value.Size > manager.Limits.MaxTotalSize+manager.Limits.MaxManifestSize {
			continue
		}
		items = append(items, Item{Name: value.Name, Size: value.Size, ModTime: value.ModTime})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name > items[j].Name })
	return items, nil
}

func (manager Manager) Upload(ctx context.Context, snapshotPath string) (Item, error) {
	profile, err := manager.preflightProfile(ctx)
	if err != nil {
		return Item{}, err
	}
	if profile.legacy {
		return Item{}, errors.New("new uploads are disabled for the legacy hidden app-folder connection; migrate it to the visible Drive folder first")
	}
	acknowledged, err := manager.recoveryAcknowledged()
	if err != nil || !acknowledged {
		return Item{}, errors.New("cloud upload is disabled until matching recovery material is stored and acknowledged")
	}
	absolute, err := filepath.Abs(snapshotPath)
	if err != nil {
		return Item{}, err
	}
	name := filepath.Base(absolute)
	if !validSnapshotName(name) {
		return Item{}, errors.New("local snapshot filename is not eligible for cloud upload")
	}
	value, err := snapshot.ValidateContext(ctx, absolute, manager.Limits)
	if err != nil {
		return Item{}, fmt.Errorf("validate local snapshot before upload: %w", err)
	}
	if expectedSnapshotName(value.CreatedUTC, value.SnapshotID) != name {
		return Item{}, errors.New("local snapshot filename does not match its validated manifest")
	}
	if _, err := manager.run(ctx, "copyto", absolute, manager.remotePath(name), "--immutable"); err != nil {
		return Item{}, fmt.Errorf("upload protected snapshot: %w", err)
	}
	if err := manager.verifyRemoteRoundtrip(ctx, absolute, name); err != nil {
		return Item{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return Item{}, err
	}
	return Item{Name: name, Size: info.Size()}, nil
}

func (manager Manager) Download(ctx context.Context, name string) (string, error) {
	if err := manager.preflight(ctx); err != nil {
		return "", err
	}
	if !validSnapshotName(name) {
		return "", errors.New("cloud snapshot name is unsafe")
	}
	if err := ensurePrivateDirectory(manager.SnapshotDirectory); err != nil {
		return "", err
	}
	temporaryName, err := randomName(".deck-snapshot-cloud-download-", ".tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := filepath.Join(manager.SnapshotDirectory, temporaryName)
	defer os.Remove(temporaryPath)
	if _, err := manager.run(ctx, "copyto", manager.remotePath(name), temporaryPath, "--immutable"); err != nil {
		return "", fmt.Errorf("download protected snapshot: %w", err)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return "", err
	}
	value, err := snapshot.ValidateContext(ctx, temporaryPath, manager.Limits)
	if err != nil {
		return "", fmt.Errorf("validate protected cloud download: %w", err)
	}
	if expectedSnapshotName(value.CreatedUTC, value.SnapshotID) != name {
		return "", errors.New("downloaded snapshot manifest does not match its cloud name")
	}
	finalPath, _, err := snapshot.PublishValidated(ctx, temporaryPath, manager.SnapshotDirectory, name, manager.Limits)
	if err != nil {
		return "", fmt.Errorf("publish validated cloud download: %w", err)
	}
	return finalPath, nil
}

// Inspect downloads one exact protected snapshot to a private temporary file,
// validates it fully, and never publishes it into the normal snapshot root.
// It exists for cloud-only snapshot browsing; callers receive only validated
// metadata and no persistent local copy is created.
func (manager Manager) Inspect(ctx context.Context, name string) (manifest.Manifest, Item, error) {
	if err := manager.preflight(ctx); err != nil {
		return manifest.Manifest{}, Item{}, err
	}
	if !validSnapshotName(name) {
		return manifest.Manifest{}, Item{}, errors.New("cloud snapshot name is unsafe")
	}
	temporaryDirectory, err := os.MkdirTemp("", "deck-snapshot-cloud-inspect-")
	if err != nil {
		return manifest.Manifest{}, Item{}, err
	}
	defer os.RemoveAll(temporaryDirectory)
	temporaryPath := filepath.Join(temporaryDirectory, name)
	if _, err := manager.run(ctx, "copyto", manager.remotePath(name), temporaryPath, "--immutable"); err != nil {
		return manifest.Manifest{}, Item{}, fmt.Errorf("download protected snapshot for inspection: %w", err)
	}
	if err := os.Chmod(temporaryPath, 0o600); err != nil {
		return manifest.Manifest{}, Item{}, err
	}
	value, err := snapshot.ValidateContext(ctx, temporaryPath, manager.Limits)
	if err != nil {
		return manifest.Manifest{}, Item{}, fmt.Errorf("validate protected snapshot for inspection: %w", err)
	}
	if expectedSnapshotName(value.CreatedUTC, value.SnapshotID) != name {
		return manifest.Manifest{}, Item{}, errors.New("downloaded snapshot manifest does not match its cloud name")
	}
	info, err := os.Stat(temporaryPath)
	if err != nil {
		return manifest.Manifest{}, Item{}, err
	}
	return value, Item{Name: name, Size: info.Size()}, nil
}

// Trash moves one exact visible-folder protected snapshot to the provider's
// Trash. It deliberately never accepts legacy remotes, wildcards, directory
// operations, or permanent-delete flags.
func (manager Manager) Trash(ctx context.Context, name string) error {
	profile, err := manager.preflightProfile(ctx)
	if err != nil {
		return err
	}
	if profile.legacy {
		return errors.New("legacy v0.1.0 cloud snapshots are read-only and cannot be moved to Trash")
	}
	if !validSnapshotName(name) {
		return errors.New("cloud snapshot name is unsafe")
	}
	if _, err := manager.run(ctx, "deletefile", manager.remotePath(name)); err != nil {
		return fmt.Errorf("move protected snapshot to Google Drive Trash: %w", err)
	}
	items, err := manager.listRemote(ctx)
	if err != nil {
		return fmt.Errorf("verify protected snapshot Trash operation: %w", err)
	}
	for _, item := range items {
		if item.Name == name {
			return errors.New("protected snapshot still appears in the active Google Drive listing after Trash operation")
		}
	}
	return nil
}

func (manager Manager) preflight(ctx context.Context) error {
	_, err := manager.preflightProfile(ctx)
	return err
}

type driveProfile struct {
	scope  string
	folder string
	legacy bool
}

func (manager Manager) preflightProfile(ctx context.Context) (driveProfile, error) {
	if err := manager.validate(); err != nil {
		return driveProfile{}, err
	}
	if err := manager.verifyRcloneVersion(ctx); err != nil {
		return driveProfile{}, err
	}
	info, err := os.Lstat(manager.ConfigPath)
	if directoryErr := validateExistingDirectory(filepath.Dir(manager.ConfigPath)); directoryErr != nil {
		return driveProfile{}, errors.New("cloud configuration path is unsafe")
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || privateFileModeError(info) != nil {
		return driveProfile{}, errors.New("cloud configuration is missing or not a private regular file")
	}
	if !manager.AllowUnencryptedTest {
		if _, err := manager.run(ctx, "config", "encryption", "check"); err != nil {
			return driveProfile{}, errors.New("cloud configuration is not encrypted or its password is invalid")
		}
	}
	crypt, err := manager.redactedRemote(ctx, manager.CryptRemote)
	if err != nil {
		return driveProfile{}, err
	}
	if crypt["type"] != "crypt" || crypt["filename_encryption"] != "standard" || crypt["directory_name_encryption"] != "true" {
		return driveProfile{}, errors.New("cloud remote is not the required fully name-encrypted crypt wrapper")
	}
	base, err := manager.redactedRemote(ctx, manager.BaseRemote)
	if err != nil {
		return driveProfile{}, err
	}
	if base["type"] != manager.ExpectedBaseType {
		return driveProfile{}, errors.New("cloud base remote has an unexpected backend type")
	}
	if manager.ExpectedBaseType != "drive" {
		if crypt["remote"] != manager.BaseRemote+":"+manager.BasePath {
			return driveProfile{}, errors.New("cloud remote is not the required fully name-encrypted crypt wrapper")
		}
		return driveProfile{}, nil
	}
	if strings.TrimSpace(base["client_id"]) == "" {
		return driveProfile{}, errors.New("Google Drive remote must use the dedicated desktop client ID")
	}
	switch {
	case base["scope"] == "drive.file" && manager.BasePath == GoogleDriveBasePath && crypt["remote"] == manager.BaseRemote+":"+GoogleDriveBasePath:
		return driveProfile{scope: "drive.file", folder: "My Drive/" + GoogleDriveBasePath}, nil
	case base["scope"] == "drive.appfolder" && crypt["remote"] == manager.BaseRemote+":":
		return driveProfile{scope: "drive.appfolder", folder: "hidden application data", legacy: true}, nil
	default:
		return driveProfile{}, errors.New("Google Drive remote must use drive.file at My Drive/Deck Snapshot/Snapshots; only the v0.1.0 hidden app-folder layout is accepted for read-only migration")
	}
}

func (manager Manager) redactedRemote(ctx context.Context, name string) (map[string]string, error) {
	result, err := manager.run(ctx, "config", "redacted", name)
	if err != nil {
		return nil, fmt.Errorf("inspect redacted cloud remote %q: %w", name, err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(result.Stdout, "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return values, nil
}

func (manager Manager) verifyRemoteRoundtrip(ctx context.Context, sourcePath, name string) error {
	directory := filepath.Dir(sourcePath)
	temporaryName, err := randomName(".deck-snapshot-cloud-verify-", ".tmp")
	if err != nil {
		return err
	}
	temporaryPath := filepath.Join(directory, temporaryName)
	defer os.Remove(temporaryPath)
	if _, err := manager.run(ctx, "copyto", manager.remotePath(name), temporaryPath, "--immutable"); err != nil {
		return fmt.Errorf("verify protected upload roundtrip: %w", err)
	}
	if _, err := snapshot.ValidateContext(ctx, temporaryPath, manager.Limits); err != nil {
		return fmt.Errorf("validate protected upload roundtrip: %w", err)
	}
	sourceHash, sourceSize, err := hashFile(sourcePath, manager.Limits.MaxTotalSize+manager.Limits.MaxManifestSize)
	if err != nil {
		return err
	}
	remoteHash, remoteSize, err := hashFile(temporaryPath, manager.Limits.MaxTotalSize+manager.Limits.MaxManifestSize)
	if err != nil || sourceHash != remoteHash || sourceSize != remoteSize {
		return errors.New("protected upload roundtrip did not reproduce the validated local snapshot")
	}
	return nil
}

func (manager Manager) verifyRemoteSnapshot(ctx context.Context, name string) error {
	if !validSnapshotName(name) {
		return errors.New("cloud snapshot name is unsafe")
	}
	if err := ensurePrivateDirectory(manager.SnapshotDirectory); err != nil {
		return err
	}
	temporaryName, err := randomName(".deck-snapshot-cloud-recovery-verify-", ".tmp")
	if err != nil {
		return err
	}
	temporaryPath := filepath.Join(manager.SnapshotDirectory, temporaryName)
	defer os.Remove(temporaryPath)
	if _, err := manager.run(ctx, "copyto", manager.remotePath(name), temporaryPath, "--immutable"); err != nil {
		return fmt.Errorf("download protected snapshot for recovery verification: %w", err)
	}
	if _, err := snapshot.ValidateContext(ctx, temporaryPath, manager.Limits); err != nil {
		return fmt.Errorf("validate protected snapshot for recovery verification: %w", err)
	}
	return nil
}

func (manager Manager) run(ctx context.Context, arguments ...string) (Result, error) {
	secrets := map[string]string{}
	if manager.ConfigPassword != "" {
		secrets["RCLONE_CONFIG_PASS"] = manager.ConfigPassword
	}
	if manager.CryptPassword != "" {
		secrets["RCLONE_CRYPT_PASSWORD"] = manager.CryptPassword
	}
	if manager.CryptPassword2 != "" {
		secrets["RCLONE_CRYPT_PASSWORD2"] = manager.CryptPassword2
	}
	timeout := defaultCommandTimeout
	if len(arguments) > 0 && arguments[0] == "copyto" {
		timeout = maximumCommandTimeout
	}
	if len(arguments) > 3 && arguments[0] == "config" && arguments[1] == "create" && arguments[3] == "drive" {
		timeout = maximumCommandTimeout
	}
	return manager.Runner.Run(ctx, Request{Args: arguments, SecretEnv: secrets, Timeout: timeout})
}

func (manager Manager) verifyRcloneVersion(ctx context.Context) error {
	result, err := manager.run(ctx, "version")
	if err != nil {
		return fmt.Errorf("verify pinned rclone version: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != expectedRcloneVersion {
		return fmt.Errorf("unsupported rclone version; expected %s", expectedRcloneVersion)
	}
	return nil
}

func (manager Manager) validate() error {
	if err := manager.validateBase(); err != nil {
		return err
	}
	if manager.CryptPassword == "" || manager.CryptPassword2 == "" || !validFingerprint(manager.ProtectionFingerprint) {
		return errors.New("cloud recovery material is not configured")
	}
	return nil
}

func (manager Manager) validateBase() error {
	if manager.Runner == nil || !filepath.IsAbs(manager.SnapshotDirectory) || !filepath.IsAbs(manager.StateDirectory) || !filepath.IsAbs(manager.ConfigPath) {
		return errors.New("cloud manager paths and runner must be configured")
	}
	if manager.RecoveryPath != "" && (!filepath.IsAbs(manager.RecoveryPath) || filepath.Clean(manager.RecoveryPath) != manager.RecoveryPath) {
		return errors.New("managed recovery path is invalid")
	}
	if manager.CryptRemote == "" || manager.BaseRemote == "" || manager.CryptRemote == manager.BaseRemote || manager.ExpectedBaseType == "" || strings.ContainsAny(manager.CryptRemote+manager.BaseRemote, ":/\\\r\n") {
		return errors.New("cloud remote identities are invalid")
	}
	if !manager.AllowUnencryptedTest && manager.ConfigPassword == "" {
		return errors.New("cloud configuration password is required")
	}
	if strings.ContainsAny(manager.BasePath, "\r\n") {
		return errors.New("cloud base path is invalid")
	}
	if manager.ExpectedBaseType == "drive" && !manager.AllowUnencryptedTest {
		if err := validateCloudPlatform(); err != nil {
			return err
		}
	}
	if err := manager.Limits.Validate(); err != nil {
		return err
	}
	return nil
}

func (manager Manager) remoteRoot() string            { return manager.CryptRemote + ":" }
func (manager Manager) remotePath(name string) string { return manager.remoteRoot() + name }

func (manager Manager) recoveryAcknowledged() (bool, error) {
	if !validFingerprint(manager.ProtectionFingerprint) {
		return false, nil
	}
	path := filepath.Join(manager.StateDirectory, "cloud", "recovery-acknowledgement.json")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	var value acknowledgement
	if err := decoder.Decode(&value); err != nil {
		return false, errors.New("cloud recovery acknowledgement is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return false, errors.New("cloud recovery acknowledgement has trailing data")
	}
	return value.Schema == acknowledgementSchema && value.Fingerprint == manager.ProtectionFingerprint, nil
}

func validSnapshotName(name string) bool {
	return len(name) <= 255 && filepath.Base(name) == name && snapshotNamePattern.MatchString(name)
}

func validFingerprint(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func expectedSnapshotName(createdUTC, snapshotID string) string {
	created, err := time.Parse(time.RFC3339Nano, createdUTC)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("deck-snapshot-%s-%s.tar.gz", created.UTC().Format("20060102T150405Z"), snapshotID)
}

func createPrivateFile(path string, value []byte) error {
	directory := filepath.Dir(path)
	if err := validateExistingDirectory(directory); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	name := filepath.Base(path)
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("private cloud file already exists with different material: %w", os.ErrExist)
		}
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = root.Remove(name)
		}
	}()
	if _, err := file.Write(value); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return syncDirectory(directory)
}

func randomName(prefix, suffix string) (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value) + suffix, nil
}

func hashFile(path string, maximum int64) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil || written > maximum {
		return "", written, errors.New("cloud snapshot exceeded the configured limit while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}
