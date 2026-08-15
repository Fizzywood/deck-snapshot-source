// Package platform derives all host-specific paths used by Deck Snapshot.
package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const appDirectory = "deck-snapshot"

// Environment supplies process environment and home-directory information.
// It is intentionally small so filesystem behavior can be tested with fake homes.
type Environment interface {
	LookupEnv(string) (string, bool)
	UserHomeDir() (string, error)
}

// OSEnvironment reads from the current process.
type OSEnvironment struct{}

func (OSEnvironment) LookupEnv(key string) (string, bool) { return os.LookupEnv(key) }
func (OSEnvironment) UserHomeDir() (string, error)        { return os.UserHomeDir() }

// Paths contains application-owned paths and detected customization roots.
// No member assumes that the current user is named deck.
type Paths struct {
	Home        string `json:"home"`
	Config      string `json:"config"`
	Data        string `json:"data"`
	State       string `json:"state"`
	Cache       string `json:"cache"`
	Snapshots   string `json:"snapshots"`
	Recovery    string `json:"recovery"`
	CloudConfig string `json:"cloud_config"`
	Steam       string `json:"steam"`
	Decky       string `json:"decky"`
}

// Resolve derives XDG and product roots without touching the filesystem.
func Resolve(env Environment) (Paths, error) {
	if env == nil {
		env = OSEnvironment{}
	}

	home, err := env.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	home, err = requireAbsolute("home directory", home)
	if err != nil {
		return Paths{}, err
	}

	config, err := xdgPath(env, "XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err != nil {
		return Paths{}, err
	}
	data, err := xdgPath(env, "XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	if err != nil {
		return Paths{}, err
	}
	state, err := xdgPath(env, "XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	if err != nil {
		return Paths{}, err
	}
	cache, err := xdgPath(env, "XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if err != nil {
		return Paths{}, err
	}

	steam, err := overridePath(env, "DECK_SNAPSHOT_STEAM_HOME", filepath.Join(home, ".local", "share", "Steam"))
	if err != nil {
		return Paths{}, err
	}
	deckyDefault := filepath.Join(home, "homebrew")
	decky, err := overridePath(env, "DECKY_HOME", deckyDefault)
	if err != nil {
		return Paths{}, err
	}
	if custom, ok := env.LookupEnv("DECK_SNAPSHOT_DECKY_HOME"); ok && custom != "" {
		decky, err = requireAbsolute("DECK_SNAPSHOT_DECKY_HOME", custom)
		if err != nil {
			return Paths{}, err
		}
	}

	config = filepath.Join(config, appDirectory)
	data = filepath.Join(data, appDirectory)
	state = filepath.Join(state, appDirectory)
	cache = filepath.Join(cache, appDirectory)

	return Paths{
		Home:        home,
		Config:      config,
		Data:        data,
		State:       state,
		Cache:       cache,
		Snapshots:   filepath.Join(data, "snapshots"),
		Recovery:    filepath.Join(state, "recovery"),
		CloudConfig: filepath.Join(config, "cloud", "rclone.conf"),
		Steam:       steam,
		Decky:       decky,
	}, nil
}

func xdgPath(env Environment, key, fallback string) (string, error) {
	value, ok := env.LookupEnv(key)
	if !ok || value == "" {
		return filepath.Clean(fallback), nil
	}
	return requireAbsolute(key, value)
}

func overridePath(env Environment, key, fallback string) (string, error) {
	value, ok := env.LookupEnv(key)
	if !ok || value == "" {
		return filepath.Clean(fallback), nil
	}
	return requireAbsolute(key, value)
}

func requireAbsolute(label, value string) (string, error) {
	if value == "" {
		return "", errors.New(label + " is empty")
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be an absolute path", label)
	}
	return filepath.Clean(value), nil
}
