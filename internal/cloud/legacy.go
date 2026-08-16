package cloud

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const LegacyConnectionDirectoryName = "legacy-v0.1.0"

// PreserveLegacyConnection retains the encrypted v0.1.0 OAuth configuration
// and its verified local unlock key without replacing a different preserved
// connection. Recovery material remains in the explicitly selected fallback
// file because the retired provider identity cannot use the managed object.
func PreserveLegacyConnection(configPath, passwordPath, destinationDirectory string) error {
	configContents, err := readPrivateRegular(configPath, 1024*1024)
	if err != nil {
		return fmt.Errorf("read legacy connection entry rclone.conf: %w", err)
	}
	passwordContents, err := readPrivateRegular(passwordPath, 1025)
	if err != nil {
		return fmt.Errorf("read legacy connection entry config-password: %w", err)
	}
	return preserveLegacyContents(configContents, passwordContents, destinationDirectory)
}

// PreserveLegacyConnectionWithPassword retains a locally verified encrypted
// legacy configuration when provider authorization is unavailable. The caller
// must first validate the password through Manager.InspectConfiguration.
func PreserveLegacyConnectionWithPassword(configPath, password, destinationDirectory string) error {
	if !validConfigPassword(password) {
		return errors.New("legacy cloud configuration password is invalid")
	}
	configContents, err := readPrivateRegular(configPath, 1024*1024)
	if err != nil {
		return fmt.Errorf("read legacy connection entry rclone.conf: %w", err)
	}
	return preserveLegacyContents(configContents, []byte(password+"\n"), destinationDirectory)
}

func preserveLegacyContents(configContents, passwordContents []byte, destinationDirectory string) error {
	if !filepath.IsAbs(destinationDirectory) || filepath.Clean(destinationDirectory) != destinationDirectory {
		return errors.New("legacy connection directory must be absolute and clean")
	}
	if err := ensurePrivateDirectory(destinationDirectory); err != nil {
		return fmt.Errorf("prepare legacy connection directory: %w", err)
	}
	entries := []struct {
		contents []byte
		name     string
		maximum  int64
	}{
		{contents: configContents, name: "rclone.conf", maximum: 1024 * 1024},
		{contents: passwordContents, name: "config-password", maximum: 1025},
	}
	for _, entry := range entries {
		if len(entry.contents) == 0 || int64(len(entry.contents)) > entry.maximum {
			return fmt.Errorf("legacy connection entry %s exceeds its bounds", entry.name)
		}
		destination := filepath.Join(destinationDirectory, entry.name)
		if err := createPrivateFile(destination, entry.contents); err != nil {
			if existing, readErr := readPrivateRegular(destination, entry.maximum); readErr == nil && bytes.Equal(existing, entry.contents) {
				continue
			}
			return fmt.Errorf("preserve legacy connection entry %s without replacement: %w", entry.name, err)
		}
	}
	return syncDirectory(destinationDirectory)
}

func readPrivateRegular(path string, maximum int64) ([]byte, error) {
	if !filepath.IsAbs(path) || maximum <= 0 {
		return nil, errors.New("private file read is not configured")
	}
	directory := filepath.Dir(path)
	if err := validateExistingDirectory(directory); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	name := filepath.Base(path)
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 0 || before.Size() > maximum || privateFileModeError(before) != nil {
		return nil, errors.New("source is not a bounded private regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("private source changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(contents)) > maximum {
		return nil, errors.New("private source exceeded its size limit")
	}
	return contents, nil
}
