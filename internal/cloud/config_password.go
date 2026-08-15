package cloud

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const configPasswordBytes = 32

// LoadConfigPassword reads the generated local rclone-configuration wrapping
// key. It is local authentication state, never user input or recovery material.
func LoadConfigPassword(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("cloud configuration-key path must be absolute and clean")
	}
	directory := filepath.Dir(path)
	if err := validateExistingDirectory(directory); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", err
	}
	defer root.Close()
	name := filepath.Base(path)
	before, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > 1025 || privateFileModeError(before) != nil {
		return "", errors.New("cloud configuration key is not a small private regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return "", errors.New("cloud configuration key changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 1026))
	if err != nil || len(contents) > 1025 {
		return "", errors.New("cloud configuration key could not be read safely")
	}
	value := strings.TrimSpace(string(contents))
	if !validConfigPassword(value) {
		return "", errors.New("cloud configuration key is invalid")
	}
	return value, nil
}

// LoadOrCreateConfigPassword creates a random private wrapping key without
// replacing an existing entry.
func LoadOrCreateConfigPassword(path string) (string, error) {
	if value, err := LoadConfigPassword(path); err == nil {
		return value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			return "", err
		}
	}
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("prepare cloud configuration-key directory: %w", err)
	}
	buffer := make([]byte, configPasswordBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate cloud configuration key: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(buffer)
	if err := createPrivateFile(path, []byte(value+"\n")); errors.Is(err, os.ErrExist) {
		return LoadConfigPassword(path)
	} else if err != nil {
		return "", fmt.Errorf("create cloud configuration key: %w", err)
	}
	return value, nil
}

// SaveConfigPassword stores a successfully verified legacy key exactly once.
func SaveConfigPassword(path, value string) error {
	if !validConfigPassword(value) {
		return errors.New("legacy cloud configuration password is invalid")
	}
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := createPrivateFile(path, []byte(value+"\n")); err != nil {
		return fmt.Errorf("save cloud configuration key: %w", err)
	}
	return nil
}

func validConfigPassword(value string) bool {
	return len(value) >= 12 && len(value) <= 1024 && !strings.ContainsAny(value, "\x00\r\n")
}

func RemoveConfigPassword(path string) error {
	return removePrivateRegularFileIfExists(path)
}
