package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
)

// PublishValidated publishes an already downloaded private temporary snapshot
// without replacement. The temporary file must be in the destination directory.
func PublishValidated(ctx context.Context, temporaryPath, directory, finalName string, resourceLimits limits.Limits) (string, manifest.Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !filepath.IsAbs(temporaryPath) || !filepath.IsAbs(directory) || filepath.Dir(temporaryPath) != filepath.Clean(directory) || filepath.Base(finalName) != finalName || finalName == "." || finalName == ".." {
		return "", manifest.Manifest{}, errors.New("snapshot publication paths are unsafe")
	}
	before, err := os.Lstat(temporaryPath)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return "", manifest.Manifest{}, errors.New("downloaded snapshot temporary identity is unsafe")
	}
	value, err := ValidateContext(ctx, temporaryPath, resourceLimits)
	if err != nil {
		return "", manifest.Manifest{}, err
	}
	validated, err := os.Lstat(temporaryPath)
	if err != nil || !samePublishedFile(before, validated) {
		return "", manifest.Manifest{}, errors.New("downloaded snapshot identity changed during validation")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return "", manifest.Manifest{}, err
	}
	defer root.Close()
	if _, err := root.Lstat(finalName); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return "", manifest.Manifest{}, errors.New("a local snapshot with the cloud name already exists")
		}
		return "", manifest.Manifest{}, err
	}
	temporaryName := filepath.Base(temporaryPath)
	if err := publishNoReplace(root, temporaryName, finalName); err != nil {
		return "", manifest.Manifest{}, fmt.Errorf("publish downloaded snapshot without replacement: %w", err)
	}
	if err := syncPublishedDirectory(root); err != nil {
		return "", manifest.Manifest{}, fmt.Errorf("durably publish downloaded snapshot: %w", err)
	}
	after, err := root.Lstat(finalName)
	if err != nil || !samePublishedFile(before, after) {
		return "", manifest.Manifest{}, errors.New("published downloaded snapshot identity changed")
	}
	return filepath.Join(directory, finalName), value, nil
}
