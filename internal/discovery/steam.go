package discovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	gridLogoPattern    = regexp.MustCompile(`^([0-9]+)\.json$`)
	gridIconPattern    = regexp.MustCompile(`^([0-9]+)_icon\.ico$`)
	libraryIconPattern = regexp.MustCompile(`^([0-9]+)_icon\.(png|jpg|jpeg)$`)
)

const (
	maxGridLogoMetadataSize = 4 << 10
	maxGridIconSize         = 1 << 20
)

type gridLogoMetadata struct {
	NVersion     *int `json:"nVersion"`
	LogoPosition *struct {
		Height *float64 `json:"nHeightPct"`
		Width  *float64 `json:"nWidthPct"`
		Pinned *string  `json:"pinnedPosition"`
	} `json:"logoPosition"`
}

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
			if appID, artworkType, supported := supportedGridSidecar(name, filepath.Join(gridRoot, name)); supported {
				fileEntry, included, err := b.addSource(filepath.Join(gridRoot, name), logical, "steam")
				if err != nil {
					return err
				}
				if !included {
					continue
				}
				if err := verifyGridSidecar(filepath.Join(gridRoot, name), fileEntry.SHA256); err != nil {
					return fmt.Errorf("verify Steam artwork sidecar %s: %w", logical, err)
				}
				b.manifest.Artwork = append(b.manifest.Artwork, manifest.Artwork{AccountID: accountID, AppID: appID, Type: artworkType, LogicalPath: logical, SHA256: fileEntry.SHA256})
				continue
			}
			b.exclude(logical, "steam", "unsupported_grid_file")
			b.warn("unsupported_grid_file", "steam", "A Steam grid file was not captured because it does not match the allowlisted artwork metadata or icon formats: "+logical)
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

func supportedGridSidecar(name, sourcePath string) (string, string, bool) {
	if matches := gridLogoPattern.FindStringSubmatch(strings.ToLower(name)); len(matches) == 2 {
		return matches[1], "logo-position", validGridLogoMetadata(sourcePath)
	}
	if matches := gridIconPattern.FindStringSubmatch(strings.ToLower(name)); len(matches) == 2 {
		return matches[1], "icon", validGridIcon(sourcePath)
	}
	return "", "", false
}

func verifyGridSidecar(sourcePath, expectedSHA256 string) error {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return err
	}
	hash, _, err := hashRegularFile(sourcePath, info)
	if err != nil {
		return err
	}
	if hash != expectedSHA256 {
		return errors.New("sidecar changed after validation")
	}
	if gridLogoPattern.MatchString(strings.ToLower(filepath.Base(sourcePath))) {
		if !validGridLogoMetadata(sourcePath) {
			return errors.New("logo metadata no longer matches the allowlist")
		}
		return nil
	}
	if gridIconPattern.MatchString(strings.ToLower(filepath.Base(sourcePath))) {
		if !validGridIcon(sourcePath) {
			return errors.New("icon no longer matches the allowlist")
		}
		return nil
	}
	return errors.New("unsupported artwork sidecar name")
}

func validGridLogoMetadata(sourcePath string) bool {
	info, err := os.Lstat(sourcePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > maxGridLogoMetadataSize {
		return false
	}
	file, err := os.Open(sourcePath)
	if err != nil {
		return false
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxGridLogoMetadataSize+1))
	decoder.DisallowUnknownFields()
	var metadata gridLogoMetadata
	if err := decoder.Decode(&metadata); err != nil {
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return false
	}
	if metadata.NVersion == nil || *metadata.NVersion != 1 || metadata.LogoPosition == nil || metadata.LogoPosition.Height == nil || metadata.LogoPosition.Width == nil || metadata.LogoPosition.Pinned == nil || math.IsNaN(*metadata.LogoPosition.Height) || math.IsInf(*metadata.LogoPosition.Height, 0) || math.IsNaN(*metadata.LogoPosition.Width) || math.IsInf(*metadata.LogoPosition.Width, 0) || *metadata.LogoPosition.Height < 0 || *metadata.LogoPosition.Height > 100 || *metadata.LogoPosition.Width < 0 || *metadata.LogoPosition.Width > 100 {
		return false
	}
	switch *metadata.LogoPosition.Pinned {
	case "UpperLeft", "UpperCenter", "UpperRight", "CenterLeft", "CenterCenter", "CenterRight", "BottomLeft", "BottomCenter", "BottomRight":
		return true
	default:
		return false
	}
}

func validGridIcon(sourcePath string) bool {
	info, err := os.Lstat(sourcePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 8 || info.Size() > maxGridIconSize {
		return false
	}
	file, err := os.Open(sourcePath)
	if err != nil {
		return false
	}
	defer file.Close()
	var header [8]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return false
	}
	return bytes.Equal(header[:], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) || bytes.Equal(header[:4], []byte{0, 0, 1, 0})
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
