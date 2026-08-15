package tests

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFakeDeckHomeContainsRequiredShapes(t *testing.T) {
	root := filepath.Join("fixtures", "deck-home")
	required := []string{
		filepath.Join("homebrew", "plugins", "fixture-plugin", "plugin.json"),
		filepath.Join("homebrew", "plugins", "fixture-plugin", "package.json"),
		filepath.Join("homebrew", "settings", "fixture-plugin", "settings.json"),
		filepath.Join("homebrew", "data", "fixture-plugin", "state.json"),
		filepath.Join("homebrew", "themes", "Fixture Theme", "theme.json"),
		filepath.Join("homebrew", "themes", "Fixture Theme", "config_USER.json"),
		filepath.Join("homebrew", "themes", "Fixture.profile", "theme.json"),
		filepath.Join(".local", "share", "Steam", "userdata", "100000001", "config", "grid", "900000001p.png"),
		filepath.Join(".local", "share", "Steam", "userdata", "100000001", "config", "shortcuts.vdf"),
		filepath.Join(".local", "share", "Steam", "userdata", "100000002", "config", "grid", "900000003_hero.png"),
		filepath.Join(".local", "share", "Steam", "appcache", "librarycache", "900000002_icon.jpg"),
	}

	for _, relative := range required {
		path := filepath.Join(root, relative)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("fixture path %q: %v", relative, err)
			continue
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			t.Errorf("fixture path %q must be a non-empty regular file", relative)
		}
	}
}
