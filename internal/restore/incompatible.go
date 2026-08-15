package restore

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
)

func splitIncompatibleSettings(paths platform.Paths, snapshotID string, plugins []pluginstore.Resolution, actions []Action) ([]Action, []PreservedSetting) {
	incompatible := make(map[string]pluginstore.Resolution)
	for _, plugin := range plugins {
		if plugin.Status == "resolved" && plugin.ResolvedVersion != "" && (plugin.SnapshotVersion == "" || plugin.SnapshotVersion != plugin.ResolvedVersion) {
			incompatible[plugin.SnapshotDirectory] = plugin
		}
	}
	remaining := make([]Action, 0, len(actions))
	preserved := make([]PreservedSetting, 0)
	for _, action := range actions {
		pluginDirectory, applies := settingPluginDirectory(action.LogicalPath)
		resolution, versionChanged := incompatible[pluginDirectory]
		if !applies || !versionChanged {
			remaining = append(remaining, action)
			continue
		}
		preserveRoot := filepath.Join(paths.State, "incompatible")
		item := PreservedSetting{
			LogicalPath:  action.LogicalPath,
			Plugin:       pluginDirectory,
			PreserveRoot: preserveRoot,
			PreservePath: filepath.Join(preserveRoot, snapshotID, filepath.FromSlash(action.LogicalPath)),
			Reason:       incompatibleReason(resolution),
			Size:         action.Size,
			SHA256:       action.SHA256,
		}
		if err := ValidateTarget(item.PreserveRoot, item.PreservePath); err != nil {
			item.Operation = "blocked"
			item.Reason += " Preservation target is unsafe: " + err.Error()
			preserved = append(preserved, item)
			continue
		}
		if err := validateWritableAncestor(filepath.Dir(item.PreservePath)); err != nil {
			item.Operation = "blocked"
			item.Reason += " Preservation target is not writable: " + err.Error()
			preserved = append(preserved, item)
			continue
		}
		info, err := os.Lstat(item.PreservePath)
		if errors.Is(err, os.ErrNotExist) {
			item.Operation = "create"
		} else if err != nil || !info.Mode().IsRegular() || isLinkOrReparsePoint(info) {
			item.Operation = "blocked"
			item.Reason += " Preservation target collides with an unsafe existing entry."
		} else {
			hash, verified, hashErr := hashRegularFile(item.PreservePath, item.Size)
			if hashErr == nil && verified.Size() == item.Size && hash == item.SHA256 {
				item.Operation = "unchanged"
			} else {
				item.Operation = "blocked"
				item.Reason += " Preservation target collides with different content."
			}
		}
		preserved = append(preserved, item)
	}
	sort.Slice(preserved, func(i, j int) bool { return preserved[i].LogicalPath < preserved[j].LogicalPath })
	return remaining, preserved
}

func incompatibleReason(resolution pluginstore.Resolution) string {
	if resolution.SnapshotVersion == "" {
		return "Snapshot settings were preserved instead of applied because the snapshot plugin version is unknown while the verified current stable version is " + resolution.ResolvedVersion + "."
	}
	return "Snapshot settings were preserved instead of applied because the verified current stable plugin version changed from " + resolution.SnapshotVersion + " to " + resolution.ResolvedVersion + "."
}

func settingPluginDirectory(logicalPath string) (string, bool) {
	for _, prefix := range []string{"decky/settings/", "decky/data/"} {
		if strings.HasPrefix(logicalPath, prefix) {
			remainder := strings.TrimPrefix(logicalPath, prefix)
			directory, _, found := strings.Cut(remainder, "/")
			return directory, found && directory != ""
		}
	}
	return "", false
}
