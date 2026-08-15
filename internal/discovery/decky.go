package discovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Fizzywood/deck-snapshot/internal/manifest"
)

type pluginMetadata struct {
	Name   string `json:"name"`
	Author string `json:"author"`
}

type packageMetadata struct {
	Version string `json:"version"`
}

func (b *builder) discoverDecky(deckyRoot string) error {
	pluginsRoot := filepath.Join(deckyRoot, "plugins")
	exists, err := ensureRealDirectory(pluginsRoot)
	if err != nil {
		return fmt.Errorf("inspect Decky plugins: %w", err)
	}
	if !exists {
		b.warn("decky_not_found", "decky", "Decky Loader was not found at the derived home path.")
		return nil
	}
	entries, err := os.ReadDir(pluginsRoot)
	if err != nil {
		return fmt.Errorf("read Decky plugins: %w", err)
	}
	loaderSettingsPath := filepath.Join(deckyRoot, "settings", "loader.json")
	if _, err := os.Lstat(loaderSettingsPath); err == nil {
		if _, _, err := b.addSource(loaderSettingsPath, "decky/settings/loader.json", "decky"); err != nil {
			return fmt.Errorf("capture Decky loader settings: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Decky loader settings: %w", err)
	}
	installed := make(map[string]struct{})
	for _, entry := range entries {
		if err := b.context.Err(); err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".deck-snapshot-") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			b.exclude("decky/metadata/plugins/"+entry.Name(), "decky", "plugin_directory_not_regular")
			continue
		}
		directory := entry.Name()
		installed[directory] = struct{}{}
		pluginPath := filepath.Join(pluginsRoot, directory, "plugin.json")
		packagePath := filepath.Join(pluginsRoot, directory, "package.json")
		var pluginJSON pluginMetadata
		if err := readJSON(pluginPath, &pluginJSON); err != nil {
			b.warn("plugin_metadata_invalid", "decky", "Plugin metadata could not be read for "+directory+".")
			pluginJSON.Name = directory
		}
		var packageJSON packageMetadata
		if err := readJSON(packagePath, &packageJSON); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.warn("plugin_version_unknown", "decky", "The installed plugin version could not be read for "+directory+".")
		}
		settingsCount, err := b.addTree(filepath.Join(deckyRoot, "settings", directory), "decky/settings/"+directory, "decky")
		if err != nil {
			return err
		}
		dataCount, err := b.addTree(filepath.Join(deckyRoot, "data", directory), "decky/data/"+directory, "decky")
		if err != nil {
			return err
		}
		b.manifest.Plugins = append(b.manifest.Plugins, manifest.Plugin{
			Directory:        directory,
			Name:             pluginJSON.Name,
			Author:           pluginJSON.Author,
			Version:          packageJSON.Version,
			SettingsCaptured: settingsCount > 0,
			DataCaptured:     dataCount > 0,
		})
		b.warn("plugin_source_unresolved", "decky", "The official store/source identity still needs resolution before restore: "+pluginJSON.Name+".")
	}
	for _, rootName := range []string{"settings", "data"} {
		orphanRoot := filepath.Join(deckyRoot, rootName)
		exists, err := ensureRealDirectory(orphanRoot)
		if err != nil {
			return fmt.Errorf("inspect Decky %s root: %w", rootName, err)
		}
		if !exists {
			continue
		}
		orphanEntries, err := os.ReadDir(orphanRoot)
		if err != nil {
			return fmt.Errorf("read Decky %s root: %w", rootName, err)
		}
		for _, entry := range orphanEntries {
			if strings.HasPrefix(entry.Name(), ".deck-snapshot-") {
				continue
			}
			if !entry.IsDir() {
				continue
			}
			if _, ok := installed[entry.Name()]; !ok {
				b.warn("orphan_plugin_state", "decky", "Unmatched Decky "+rootName+" state was not captured: "+entry.Name()+".")
			}
		}
	}
	return nil
}
