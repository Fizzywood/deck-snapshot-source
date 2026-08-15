// Package config owns the versioned, non-secret application configuration.
package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Fizzywood/deck-snapshot/internal/platform"
)

const CurrentSchemaVersion = 2

// Config contains only non-secret preferences. OAuth and rclone state are
// deliberately managed outside this file.
type Config struct {
	SchemaVersion     int    `json:"schema_version"`
	LogLevel          string `json:"log_level"`
	SnapshotDirectory string `json:"snapshot_directory"`
	AutoUpload        bool   `json:"auto_upload"`
	RecoveryFile      string `json:"recovery_file,omitempty"`
}

func Default(paths platform.Paths) Config {
	return Config{
		SchemaVersion:     CurrentSchemaVersion,
		LogLevel:          "info",
		SnapshotDirectory: paths.Snapshots,
		AutoUpload:        true,
	}
}

func Path(paths platform.Paths) string { return filepath.Join(paths.Config, "config.json") }

// Load reads a strict JSON configuration. A missing file returns defaults.
func Load(path string, defaults Config) (Config, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return defaults, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()

	contents, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	if len(contents) > 1024*1024 {
		return Config{}, errors.New("configuration exceeds the size limit")
	}
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(contents, &header); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	var result Config
	switch header.SchemaVersion {
	case 1:
		var legacy struct {
			SchemaVersion     int    `json:"schema_version"`
			LogLevel          string `json:"log_level"`
			SnapshotDirectory string `json:"snapshot_directory"`
		}
		if err := decodeStrict(contents, &legacy); err != nil {
			return Config{}, err
		}
		result = defaults
		result.SchemaVersion = CurrentSchemaVersion
		result.LogLevel = legacy.LogLevel
		result.SnapshotDirectory = legacy.SnapshotDirectory
	case CurrentSchemaVersion:
		if err := decodeStrict(contents, &result); err != nil {
			return Config{}, err
		}
	default:
		return Config{}, fmt.Errorf("unsupported configuration schema version %d", header.SchemaVersion)
	}
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func (c Config) Validate() error {
	if c.SchemaVersion != CurrentSchemaVersion {
		return fmt.Errorf("unsupported configuration schema version %d", c.SchemaVersion)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("unsupported log level %q", c.LogLevel)
	}
	if !filepath.IsAbs(c.SnapshotDirectory) {
		return errors.New("snapshot_directory must be an absolute path")
	}
	if c.RecoveryFile != "" && (!filepath.IsAbs(c.RecoveryFile) || filepath.Clean(c.RecoveryFile) != c.RecoveryFile || strings.ContainsAny(c.RecoveryFile, "\x00\r\n")) {
		return errors.New("recovery_file must be an absolute clean path")
	}
	return nil
}

// Save atomically replaces the non-secret configuration inside a validated
// private directory. It never accepts credentials or recovery-key contents.
func Save(path string, value Config) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("configuration path must be absolute and clean")
	}
	if err := value.Validate(); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return fmt.Errorf("prepare configuration directory: %w", err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fmt.Errorf("open configuration directory: %w", err)
	}
	defer root.Close()
	name := filepath.Base(path)
	if info, statErr := root.Lstat(name); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("refusing to replace an unsafe configuration path")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect configuration path: %w", statErr)
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	encoded = append(encoded, '\n')
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("generate configuration temporary name: %w", err)
	}
	temporaryName := ".deck-snapshot-config-" + hex.EncodeToString(random) + ".tmp"
	temporary, err := root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private configuration temporary file: %w", err)
	}
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = root.Remove(temporaryName)
		}
	}()
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close configuration: %w", err)
	}
	if err := root.Rename(temporaryName, name); err != nil {
		return fmt.Errorf("publish configuration: %w", err)
	}
	removeTemporary = false
	if err := syncConfigDirectory(directory); err != nil {
		return fmt.Errorf("durably publish configuration: %w", err)
	}
	return nil
}

func decodeStrict(contents []byte, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(contents), int64(len(contents))))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	return ensureEOF(decoder)
}

func ensurePrivateDirectory(directory string) error {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("configuration directory must be absolute and clean")
	}
	volume := filepath.VolumeName(directory)
	anchor := volume + string(os.PathSeparator)
	if volume == "" {
		anchor = string(os.PathSeparator)
	}
	relative, err := filepath.Rel(anchor, directory)
	if err != nil || relative == "." || relative == "" {
		return errors.New("refusing to use a filesystem root as the configuration directory")
	}
	current := anchor
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		if component == "" || component == "." || component == ".." {
			return errors.New("configuration directory contains an unsafe component")
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("configuration directory contains an unsafe path component")
		}
	}
	return os.Chmod(directory, 0o700)
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("configuration contains multiple JSON values")
	}
	return fmt.Errorf("decode configuration trailer: %w", err)
}
