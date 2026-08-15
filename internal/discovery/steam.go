package discovery

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Fizzywood/deck-snapshot/internal/manifest"
)

var (
	numericNamePattern = regexp.MustCompile(`^[0-9]+$`)
	gridNamePattern    = regexp.MustCompile(`^([0-9]+)(p|_hero|_logo|_icon)?\.(png|jpg|jpeg)$`)
	libraryIconPattern = regexp.MustCompile(`^([0-9]+)_icon\.(png|jpg|jpeg)$`)
)

func (b *builder) discoverSteam(steamRoot string) error {
	userdataRoot := filepath.Join(steamRoot, "userdata")
	exists, err := ensureRealDirectory(userdataRoot)
	if err != nil {
		return fmt.Errorf("inspect Steam userdata: %w", err)
	}
	if !exists {
		b.warn("steam_userdata_not_found", "steam", "Steam userdata was not found at the derived Steam path.")
	} else if entries, readErr := os.ReadDir(userdataRoot); readErr != nil {
		return fmt.Errorf("read Steam userdata: %w", readErr)
	} else {
		for _, entry := range entries {
			if !numericNamePattern.MatchString(entry.Name()) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			accountID := entry.Name()
			b.manifest.Accounts = append(b.manifest.Accounts, manifest.SteamAccount{ID: accountID})
			gridRoot := filepath.Join(userdataRoot, accountID, "config", "grid")
			if err := b.discoverGrid(accountID, gridRoot); err != nil {
				return err
			}
			shortcutPath := filepath.Join(userdataRoot, accountID, "config", "shortcuts.vdf")
			if info, err := os.Lstat(shortcutPath); err == nil && info.Mode().IsRegular() {
				logical := path.Join("steam/metadata/userdata", accountID, "shortcuts.vdf")
				b.exclude(logical, "steam", "shortcuts_vdf_requires_safe_merge")
				b.warn("shortcuts_vdf_not_captured", "steam", "Non-Steam shortcut metadata was not captured because a safe preserving merge is not implemented.")
			}
		}
	}
	return b.discoverLibraryIcons(filepath.Join(steamRoot, "appcache", "librarycache"))
}

func (b *builder) discoverGrid(accountID, gridRoot string) error {
	exists, err := ensureRealDirectory(gridRoot)
	if err != nil {
		return fmt.Errorf("inspect Steam grid for account %s: %w", accountID, err)
	}
	if !exists {
		return nil
	}
	entries, err := os.ReadDir(gridRoot)
	if err != nil {
		return fmt.Errorf("read Steam grid for account %s: %w", accountID, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		extension := strings.ToLower(filepath.Ext(name))
		if extension != ".png" && extension != ".jpg" && extension != ".jpeg" {
			logical := path.Join("steam/artwork/userdata", accountID, "grid", name)
			b.exclude(logical, "steam", "unsupported_grid_file")
			b.warn("unsupported_grid_file", "steam", "A non-image Steam grid file was not captured because its current semantics are unverified: "+logical)
			continue
		}
		logical := path.Join("steam/artwork/userdata", accountID, "grid", name)
		fileEntry, included, err := b.addSource(filepath.Join(gridRoot, name), logical, "steam")
		if err != nil {
			return err
		}
		if !included {
			continue
		}
		appID, artworkType := classifyGridName(name)
		if artworkType == "unknown" {
			b.warn("artwork_type_unverified", "steam", "An artwork file was captured with an unverified type: "+logical)
		}
		b.manifest.Artwork = append(b.manifest.Artwork, manifest.Artwork{
			AccountID:   accountID,
			AppID:       appID,
			Type:        artworkType,
			LogicalPath: logical,
			SHA256:      fileEntry.SHA256,
		})
	}
	return nil
}

func (b *builder) discoverLibraryIcons(root string) error {
	exists, err := ensureRealDirectory(root)
	if err != nil {
		return fmt.Errorf("inspect Steam library cache: %w", err)
	}
	if !exists {
		return nil
	}
	directory, err := os.Open(root)
	if err != nil {
		return fmt.Errorf("open Steam library cache: %w", err)
	}
	defer directory.Close()
	for {
		entries, err := directory.ReadDir(256)
		for _, entry := range entries {
			matches := libraryIconPattern.FindStringSubmatch(strings.ToLower(entry.Name()))
			if len(matches) == 0 || entry.IsDir() {
				continue
			}
			logical := path.Join("steam/artwork/librarycache", entry.Name())
			fileEntry, included, addErr := b.addSource(filepath.Join(root, entry.Name()), logical, "steam")
			if addErr != nil {
				return addErr
			}
			if included {
				b.manifest.Artwork = append(b.manifest.Artwork, manifest.Artwork{AppID: matches[1], Type: "icon", LogicalPath: logical, SHA256: fileEntry.SHA256})
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read Steam library cache: %w", err)
		}
	}
	return nil
}

func classifyGridName(name string) (string, string) {
	matches := gridNamePattern.FindStringSubmatch(strings.ToLower(name))
	if len(matches) == 0 {
		return "", "unknown"
	}
	switch matches[2] {
	case "p":
		return matches[1], "portrait"
	case "_hero":
		return matches[1], "hero"
	case "_logo":
		return matches[1], "logo"
	case "_icon":
		return matches[1], "icon"
	case "":
		return matches[1], "grid"
	default:
		return matches[1], "unknown"
	}
}
