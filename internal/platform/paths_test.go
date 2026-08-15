package platform

import (
	"errors"
	"path/filepath"
	"testing"
)

type fakeEnvironment struct {
	home   string
	values map[string]string
	err    error
}

func (f fakeEnvironment) LookupEnv(key string) (string, bool) {
	value, ok := f.values[key]
	return value, ok
}

func (f fakeEnvironment) UserHomeDir() (string, error) { return f.home, f.err }

func TestResolveUsesDerivedHomeAndXDGDefaults(t *testing.T) {
	home := filepath.Join(t.TempDir(), "alex")
	paths, err := Resolve(fakeEnvironment{home: home, values: map[string]string{}})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}

	wantDecky := filepath.Join(home, "homebrew")
	wantSteam := filepath.Join(home, ".local", "share", "Steam")
	wantConfig := filepath.Join(home, ".config", appDirectory)
	if paths.Decky != wantDecky || paths.Steam != wantSteam || paths.Config != wantConfig {
		t.Fatalf("unexpected paths: %#v", paths)
	}
	if paths.Home == "/home/deck" {
		t.Fatal("Resolve() hardcoded the SteamOS username")
	}
}

func TestResolveHonorsAbsoluteOverrides(t *testing.T) {
	root := t.TempDir()
	env := fakeEnvironment{
		home: filepath.Join(root, "home"),
		values: map[string]string{
			"XDG_CONFIG_HOME":          filepath.Join(root, "config"),
			"XDG_DATA_HOME":            filepath.Join(root, "data"),
			"XDG_STATE_HOME":           filepath.Join(root, "state"),
			"XDG_CACHE_HOME":           filepath.Join(root, "cache"),
			"DECK_SNAPSHOT_STEAM_HOME": filepath.Join(root, "steam"),
			"DECK_SNAPSHOT_DECKY_HOME": filepath.Join(root, "decky"),
		},
	}

	paths, err := Resolve(env)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if paths.Steam != filepath.Join(root, "steam") || paths.Decky != filepath.Join(root, "decky") {
		t.Fatalf("overrides not honored: %#v", paths)
	}
}

func TestResolveRejectsRelativeXDGPath(t *testing.T) {
	_, err := Resolve(fakeEnvironment{
		home:   filepath.Join(t.TempDir(), "alex"),
		values: map[string]string{"XDG_CONFIG_HOME": "relative/config"},
	})
	if err == nil {
		t.Fatal("Resolve() accepted a relative XDG path")
	}
}

func TestResolvePropagatesHomeError(t *testing.T) {
	want := errors.New("no user")
	_, err := Resolve(fakeEnvironment{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("Resolve() error = %v, want wrapped %v", err, want)
	}
}
