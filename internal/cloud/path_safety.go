package cloud

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func ensurePrivateDirectory(directory string) error {
	components, current, err := absolutePathComponents(directory)
	if err != nil {
		return err
	}
	if len(components) == 0 {
		return errors.New("refusing to use a filesystem root as a private cloud directory")
	}
	for _, component := range components {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			if mkdirErr := os.Mkdir(current, 0o700); mkdirErr != nil {
				return mkdirErr
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("private cloud directory contains an unsafe path component")
		}
	}
	return os.Chmod(filepath.Clean(directory), 0o700)
}

func validateExistingDirectory(directory string) error {
	components, current, err := absolutePathComponents(directory)
	if err != nil {
		return err
	}
	for _, component := range components {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("cloud path contains an unsafe directory component")
		}
	}
	return nil
}

func absolutePathComponents(path string) ([]string, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, "", errors.New("cloud path must be absolute and clean")
	}
	volume := filepath.VolumeName(path)
	anchor := volume + string(os.PathSeparator)
	if volume == "" {
		anchor = string(os.PathSeparator)
	}
	relative, err := filepath.Rel(anchor, path)
	if err != nil || relative == "." || relative == "" {
		if err != nil {
			return nil, "", err
		}
		return nil, anchor, nil
	}
	components := strings.Split(relative, string(os.PathSeparator))
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, "", errors.New("cloud path contains an unsafe component")
		}
	}
	return components, anchor, nil
}
