package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Fizzywood/deck-snapshot/internal/manifest"
)

type themeMetadata struct {
	Name            string   `json:"name"`
	DisplayName     string   `json:"display_name"`
	Version         string   `json:"version"`
	ManifestVersion int      `json:"manifest_version"`
	Flags           []string `json:"flags"`
}

type themeConfig struct {
	Active *bool `json:"active"`
}

func (b *builder) discoverCSS(deckyRoot string) error {
	themesRoot := filepath.Join(deckyRoot, "themes")
	exists, err := ensureRealDirectory(themesRoot)
	if err != nil {
		return fmt.Errorf("inspect CSS Loader themes: %w", err)
	}
	if !exists {
		b.warn("css_loader_state_not_found", "css-loader", "CSS Loader theme state was not found.")
		return nil
	}
	entries, err := os.ReadDir(themesRoot)
	if err != nil {
		return fmt.Errorf("read CSS Loader themes: %w", err)
	}
	if _, err := b.addTree(themesRoot, "css-loader/themes", "css-loader"); err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		directory := entry.Name()
		var metadata themeMetadata
		if err := readJSON(filepath.Join(themesRoot, directory, "theme.json"), &metadata); err != nil {
			b.warn("css_theme_metadata_invalid", "css-loader", "Theme metadata could not be read for "+directory+".")
			continue
		}
		profile := strings.HasSuffix(directory, ".profile")
		for _, flag := range metadata.Flags {
			if strings.EqualFold(flag, "PRESET") {
				profile = true
			}
		}
		var active *bool
		for _, configName := range []string{"config_USER.json", "config_ROOT.json"} {
			var config themeConfig
			if err := readJSON(filepath.Join(themesRoot, directory, configName), &config); err == nil && config.Active != nil {
				active = config.Active
				break
			}
		}
		b.manifest.CSSThemes = append(b.manifest.CSSThemes, manifest.CSSTheme{
			Directory:       directory,
			Name:            metadata.Name,
			DisplayName:     metadata.DisplayName,
			Version:         metadata.Version,
			ManifestVersion: metadata.ManifestVersion,
			Profile:         profile,
			Active:          active,
		})
	}
	return nil
}
