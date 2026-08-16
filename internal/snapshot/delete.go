package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
)

var userSnapshotName = regexp.MustCompile(`^deck-snapshot-[0-9]{8}T[0-9]{6}Z-[A-Za-z0-9._-]+\.tar\.gz$`)

// DeleteValidated removes one exact validated user snapshot from its canonical
// snapshot root. The caller supplies a filename, never a path, so reports,
// recovery archives, configuration, and arbitrary filesystem targets cannot
// be selected through this destructive boundary.
func DeleteValidated(ctx context.Context, directory, name string, resourceLimits limits.Limits) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || !userSnapshotName.MatchString(name) || filepath.Base(name) != name {
		return errors.New("snapshot deletion target is unsafe")
	}
	rootInfo, err := os.Lstat(directory)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("snapshot directory is not a real directory")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return fmt.Errorf("open snapshot directory: %w", err)
	}
	defer root.Close()
	before, err := root.Lstat(name)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return errors.New("selected snapshot is not a regular file")
	}
	path := filepath.Join(directory, name)
	if _, err := ValidateContext(ctx, path, resourceLimits); err != nil {
		return fmt.Errorf("selected snapshot failed validation: %w", err)
	}
	after, err := root.Lstat(name)
	if err != nil || !samePublishedFile(before, after) {
		return errors.New("selected snapshot changed during validation")
	}
	if err := root.Remove(name); err != nil {
		return fmt.Errorf("remove selected snapshot: %w", err)
	}
	if err := syncPublishedDirectory(root); err != nil {
		return fmt.Errorf("durably remove selected snapshot: %w", err)
	}
	return nil
}
