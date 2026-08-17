package restore

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/discovery"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
)

// buildConvergenceActions discovers only already-supported live CSS Loader and
// artwork entries.  It never treats unknown, excluded, or orphaned material as
// removable state.  Every returned removal is an exact regular file already
// protected by the normal immutable-plan, recovery, and rollback machinery.
func buildConvergenceActions(ctx context.Context, paths platform.Paths, snapshotValue manifest.Manifest, appVersion string, now time.Time, resourceLimits limits.Limits) ([]Action, error) {
	current, err := discovery.Discover(ctx, discovery.Options{
		Paths: paths, AppVersion: appVersion, DeviceID: "restore-convergence", DeviceName: "Restore convergence",
		SnapshotID: "dsnap-convergence", Now: now, Limits: resourceLimits,
	})
	if err != nil {
		return nil, fmt.Errorf("discover supported live state for convergence: %w", err)
	}
	desiredFiles := make(map[string]struct{}, len(snapshotValue.Files))
	for _, entry := range snapshotValue.Files {
		desiredFiles[entry.LogicalPath] = struct{}{}
	}
	desiredThemes := make(map[string]struct{}, len(snapshotValue.CSSThemes))
	for _, theme := range snapshotValue.CSSThemes {
		desiredThemes[theme.Directory] = struct{}{}
	}
	candidates := make(map[string]manifest.File, len(current.Candidates))
	for _, candidate := range current.Candidates {
		candidates[candidate.Entry.LogicalPath] = candidate.Entry
	}
	removals := make(map[string]Action)
	add := func(entry manifest.File) error {
		if _, present := desiredFiles[entry.LogicalPath]; present {
			return nil
		}
		action, mapped, err := buildAction(paths, entry, resourceLimits)
		if err != nil || !mapped || action.Operation != "unchanged" {
			if err == nil {
				err = fmt.Errorf("live target is not an unchanged safe regular file")
			}
			return fmt.Errorf("prepare convergence removal for %q: %w", entry.LogicalPath, err)
		}
		action.Operation = "remove"
		removals[action.LogicalPath] = action
		return nil
	}
	for _, theme := range current.Manifest.CSSThemes {
		if _, wanted := desiredThemes[theme.Directory]; wanted {
			continue
		}
		prefix := "css-loader/themes/" + theme.Directory + "/"
		for _, exclusion := range current.Manifest.Exclusions {
			if strings.HasPrefix(exclusion.LogicalPath, prefix) {
				return nil, fmt.Errorf("refuse to remove CSS Loader theme %q with excluded or unsafe content", theme.Directory)
			}
		}
		found := false
		for logicalPath, entry := range candidates {
			if strings.HasPrefix(logicalPath, prefix) {
				found = true
				if err := add(entry); err != nil {
					return nil, err
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("refuse to remove CSS Loader theme %q without a complete supported file inventory", theme.Directory)
		}
	}
	for _, artwork := range current.Manifest.Artwork {
		if _, wanted := desiredFiles[artwork.LogicalPath]; wanted {
			continue
		}
		entry, found := candidates[artwork.LogicalPath]
		if !found {
			return nil, fmt.Errorf("refuse to remove Steam artwork %q without a validated source file", artwork.LogicalPath)
		}
		if !strings.HasPrefix(entry.LogicalPath, "steam/artwork/") || path.Clean(entry.LogicalPath) != entry.LogicalPath {
			return nil, fmt.Errorf("refuse unsafe Steam artwork convergence path %q", entry.LogicalPath)
		}
		if err := add(entry); err != nil {
			return nil, err
		}
	}
	result := make([]Action, 0, len(removals))
	for _, action := range removals {
		result = append(result, action)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LogicalPath < result[j].LogicalPath })
	return result, nil
}
