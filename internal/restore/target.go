package restore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ValidateTarget(root, target string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(target) {
		return errors.New("restore root and target must be absolute")
	}
	if strings.HasPrefix(root, `\\`) || strings.HasPrefix(target, `\\`) || strings.HasPrefix(root, "//") || strings.HasPrefix(target, "//") {
		return errors.New("UNC/network restore targets are not allowed")
	}
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("restore target escapes or aliases its allowlisted root")
	}
	if err := validateExistingDirectoryComponents(filepath.Dir(target)); err != nil {
		return err
	}
	return nil
}

func validateWritableAncestor(directory string) error {
	probe, err := nearestExistingDirectory(directory)
	if err != nil {
		return err
	}
	return platformDirectoryWritable(probe)
}

func validateExistingDirectoryComponents(directory string) error {
	directory = filepath.Clean(directory)
	volume := filepath.VolumeName(directory)
	remainder := strings.TrimPrefix(directory, volume)
	remainder = strings.TrimPrefix(remainder, string(filepath.Separator))
	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect target path component: %w", err)
		}
		if isLinkOrReparsePoint(info) || !info.IsDir() {
			return fmt.Errorf("target path component is not a real directory: %s", current)
		}
	}
	return nil
}

func validateOwnedPathBeneathHome(home, target string) error {
	homeInfo, err := os.Lstat(home)
	if err != nil || !homeInfo.IsDir() || isLinkOrReparsePoint(homeInfo) {
		return errors.New("target home is not a real directory")
	}
	if err := platformOwnedByCurrentUser(homeInfo); err != nil {
		return fmt.Errorf("target home ownership is unsafe: %w", err)
	}
	relative, err := relativeToHome(home, filepath.Join(target, ".deck-snapshot-owned-scope"))
	if err != nil {
		return err
	}
	current := home
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || isLinkOrReparsePoint(info) {
			return fmt.Errorf("target home descendant is not a real directory: %s", current)
		}
		if err := platformOwnedByCurrentUser(info); err != nil {
			return fmt.Errorf("target home descendant ownership is unsafe: %s: %w", current, err)
		}
	}
	return nil
}
