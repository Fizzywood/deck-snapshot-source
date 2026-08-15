// Package identity manages the non-sensitive local device identifier.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var deviceIDPattern = regexp.MustCompile(`^ds-[a-f0-9]{32}$`)

func LoadOrCreate(stateDirectory string) (string, error) {
	if !filepath.IsAbs(stateDirectory) {
		return "", errors.New("state directory must be absolute")
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create state directory: %w", err)
	}
	path := filepath.Join(stateDirectory, "device-id")
	if value, err := read(path); err == nil {
		return value, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate device id: %w", err)
	}
	value := "ds-" + hex.EncodeToString(buffer)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return read(path)
	}
	if err != nil {
		return "", fmt.Errorf("create device id: %w", err)
	}
	remove := true
	defer func() {
		file.Close()
		if remove {
			os.Remove(path)
		}
	}()
	if _, err := file.WriteString(value + "\n"); err != nil {
		return "", fmt.Errorf("write device id: %w", err)
	}
	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("sync device id: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close device id: %w", err)
	}
	remove = false
	return value, nil
}

func read(path string) (string, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() > 128 {
		return "", errors.New("stored device id is not a small regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return "", errors.New("stored device id changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil {
		return "", err
	}
	if len(contents) > 128 {
		return "", errors.New("stored device id is too large")
	}
	value := strings.TrimSpace(string(contents))
	if !deviceIDPattern.MatchString(value) {
		return "", errors.New("stored device id is invalid")
	}
	return value, nil
}
