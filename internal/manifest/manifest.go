// Package manifest defines and validates snapshot manifest version 1.
package manifest

import (
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const FormatVersion = "1.0"

var windowsDrivePattern = regexp.MustCompile(`^[A-Za-z]:`)
var snapshotIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var numericIDPattern = regexp.MustCompile(`^[0-9]+$`)

type Manifest struct {
	FormatVersion string         `json:"format_version"`
	SnapshotID    string         `json:"snapshot_id"`
	AppVersion    string         `json:"app_version"`
	CreatedUTC    string         `json:"created_utc"`
	CreatedLocal  string         `json:"created_local"`
	Device        Device         `json:"device"`
	Host          Host           `json:"host"`
	Detected      Detected       `json:"detected"`
	Accounts      []SteamAccount `json:"steam_accounts"`
	Plugins       []Plugin       `json:"plugins"`
	CSSThemes     []CSSTheme     `json:"css_themes"`
	Artwork       []Artwork      `json:"artwork"`
	Files         []File         `json:"files"`
	Exclusions    []Exclusion    `json:"exclusions"`
	Warnings      []Warning      `json:"warnings"`
	Compatibility Compatibility  `json:"compatibility"`
}

type Device struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type Host struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type Detected struct {
	SteamOSVersion string `json:"steamos_version,omitempty"`
	DeckyVersion   string `json:"decky_version,omitempty"`
}

type SteamAccount struct {
	ID string `json:"id"`
}

type Plugin struct {
	Directory        string `json:"directory"`
	Name             string `json:"name"`
	Author           string `json:"author,omitempty"`
	Version          string `json:"version,omitempty"`
	SourceIdentity   string `json:"source_identity,omitempty"`
	SettingsCaptured bool   `json:"settings_captured"`
	DataCaptured     bool   `json:"data_captured"`
}

type CSSTheme struct {
	Directory       string `json:"directory"`
	Name            string `json:"name"`
	DisplayName     string `json:"display_name,omitempty"`
	Version         string `json:"version,omitempty"`
	ManifestVersion int    `json:"manifest_version,omitempty"`
	Profile         bool   `json:"profile"`
	Active          *bool  `json:"active,omitempty"`
}

type Artwork struct {
	AccountID   string `json:"account_id,omitempty"`
	AppID       string `json:"app_id,omitempty"`
	Type        string `json:"type"`
	LogicalPath string `json:"logical_path"`
	SHA256      string `json:"sha256"`
}

type File struct {
	LogicalPath string `json:"logical_path"`
	Component   string `json:"component"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	Mode        uint32 `json:"mode"`
	Generated   bool   `json:"generated,omitempty"`
}

type Exclusion struct {
	LogicalPath string `json:"logical_path,omitempty"`
	Component   string `json:"component"`
	Reason      string `json:"reason"`
}

type Warning struct {
	Code      string `json:"code"`
	Component string `json:"component"`
	Message   string `json:"message"`
}

type Compatibility struct {
	MinimumAppVersion          string   `json:"minimum_app_version"`
	HardwareValidationRequired bool     `json:"hardware_validation_required"`
	UnverifiedBehaviors        []string `json:"unverified_behaviors"`
}

func New(snapshotID, appVersion, deviceID, deviceName string, now time.Time) Manifest {
	return Manifest{
		FormatVersion: FormatVersion,
		SnapshotID:    snapshotID,
		AppVersion:    appVersion,
		CreatedUTC:    now.UTC().Format(time.RFC3339Nano),
		CreatedLocal:  now.Format(time.RFC3339Nano),
		Device:        Device{ID: deviceID, Name: deviceName},
		Host:          Host{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Compatibility: Compatibility{
			MinimumAppVersion:          "0.1.0",
			HardwareValidationRequired: true,
			UnverifiedBehaviors: []string{
				"Steam client artwork filename semantics",
				"Steam and Decky restart behavior",
				"Non-Steam shortcut identity matching",
			},
		},
	}
}

func (m *Manifest) Normalize() {
	sort.Slice(m.Accounts, func(i, j int) bool { return m.Accounts[i].ID < m.Accounts[j].ID })
	sort.Slice(m.Plugins, func(i, j int) bool { return m.Plugins[i].Directory < m.Plugins[j].Directory })
	sort.Slice(m.CSSThemes, func(i, j int) bool { return m.CSSThemes[i].Directory < m.CSSThemes[j].Directory })
	sort.Slice(m.Artwork, func(i, j int) bool { return m.Artwork[i].LogicalPath < m.Artwork[j].LogicalPath })
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].LogicalPath < m.Files[j].LogicalPath })
	sort.Slice(m.Exclusions, func(i, j int) bool {
		if m.Exclusions[i].LogicalPath == m.Exclusions[j].LogicalPath {
			return m.Exclusions[i].Reason < m.Exclusions[j].Reason
		}
		return m.Exclusions[i].LogicalPath < m.Exclusions[j].LogicalPath
	})
	sort.Slice(m.Warnings, func(i, j int) bool {
		if m.Warnings[i].Code == m.Warnings[j].Code {
			return m.Warnings[i].Message < m.Warnings[j].Message
		}
		return m.Warnings[i].Code < m.Warnings[j].Code
	})
	sort.Strings(m.Compatibility.UnverifiedBehaviors)
}

func (m Manifest) Validate(maxPathLength int) error {
	if majorVersion(m.FormatVersion) != "1" {
		return fmt.Errorf("unsupported snapshot format version %q", m.FormatVersion)
	}
	if !snapshotIDPattern.MatchString(m.SnapshotID) || m.SnapshotID == "." || m.SnapshotID == ".." {
		return errors.New("snapshot_id is missing or unsafe")
	}
	if m.AppVersion == "" {
		return errors.New("app_version is missing")
	}
	if _, err := time.Parse(time.RFC3339Nano, m.CreatedUTC); err != nil {
		return fmt.Errorf("created_utc is invalid: %w", err)
	}
	if _, err := time.Parse(time.RFC3339Nano, m.CreatedLocal); err != nil {
		return fmt.Errorf("created_local is invalid: %w", err)
	}
	if m.Device.ID == "" {
		return errors.New("device id is missing")
	}
	if containsControl(m.Device.ID + m.Device.Name + m.AppVersion) {
		return errors.New("device or app metadata contains control characters")
	}
	if m.Host.OS == "" || m.Host.Arch == "" {
		return errors.New("host platform is missing")
	}
	if len(m.Detected.SteamOSVersion) > 128 || len(m.Detected.DeckyVersion) > 128 || containsControl(m.Detected.SteamOSVersion+m.Detected.DeckyVersion) {
		return errors.New("detected version metadata is unsafe")
	}
	seen := make(map[string]struct{}, len(m.Files))
	for _, file := range m.Files {
		if err := ValidateLogicalPath(file.LogicalPath, maxPathLength); err != nil {
			return fmt.Errorf("file path %q: %w", file.LogicalPath, err)
		}
		if _, exists := seen[file.LogicalPath]; exists {
			return fmt.Errorf("duplicate file path %q", file.LogicalPath)
		}
		seen[file.LogicalPath] = struct{}{}
		if file.Size < 0 {
			return fmt.Errorf("file %q has a negative size", file.LogicalPath)
		}
		if !validSHA256(file.SHA256) {
			return fmt.Errorf("file %q has an invalid SHA-256", file.LogicalPath)
		}
		if file.Component == "" {
			return fmt.Errorf("file %q has no component", file.LogicalPath)
		}
	}
	seenExclusions := make(map[string]struct{}, len(m.Exclusions))
	for _, exclusion := range m.Exclusions {
		if exclusion.Component == "" || exclusion.Reason == "" {
			return errors.New("exclusion is missing its component or reason")
		}
		if exclusion.LogicalPath != "" {
			if err := ValidateLogicalPath(exclusion.LogicalPath, maxPathLength); err != nil {
				return fmt.Errorf("exclusion path %q: %w", exclusion.LogicalPath, err)
			}
		}
		key := exclusion.Component + "\x00" + exclusion.LogicalPath + "\x00" + exclusion.Reason
		if _, exists := seenExclusions[key]; exists {
			return errors.New("duplicate exclusion metadata")
		}
		seenExclusions[key] = struct{}{}
	}
	seenWarnings := make(map[string]struct{}, len(m.Warnings))
	for _, warning := range m.Warnings {
		if warning.Code == "" || warning.Component == "" || warning.Message == "" || containsControl(warning.Code+warning.Component+warning.Message) {
			return errors.New("warning is missing fields or contains control characters")
		}
		key := warning.Code + "\x00" + warning.Component + "\x00" + warning.Message
		if _, exists := seenWarnings[key]; exists {
			return errors.New("duplicate warning metadata")
		}
		seenWarnings[key] = struct{}{}
	}
	seenAccounts := make(map[string]struct{}, len(m.Accounts))
	for _, account := range m.Accounts {
		if !numericIDPattern.MatchString(account.ID) {
			return fmt.Errorf("invalid Steam account id %q", account.ID)
		}
		if _, exists := seenAccounts[account.ID]; exists {
			return fmt.Errorf("duplicate Steam account id %q", account.ID)
		}
		seenAccounts[account.ID] = struct{}{}
	}
	seenPlugins := make(map[string]string, len(m.Plugins))
	for _, plugin := range m.Plugins {
		if plugin.Name == "" || containsControl(plugin.Directory+plugin.Name+plugin.Author+plugin.Version+plugin.SourceIdentity) {
			return errors.New("plugin name is missing")
		}
		if err := ValidateLogicalPath("plugin/"+plugin.Directory, maxPathLength); err != nil {
			return fmt.Errorf("plugin directory %q: %w", plugin.Directory, err)
		}
		folded := strings.ToLower(plugin.Directory)
		if previous, exists := seenPlugins[folded]; exists {
			return fmt.Errorf("duplicate or case-colliding plugin directories %q and %q", previous, plugin.Directory)
		}
		seenPlugins[folded] = plugin.Directory
	}
	seenThemes := make(map[string]string, len(m.CSSThemes))
	for _, theme := range m.CSSThemes {
		if theme.Name == "" || containsControl(theme.Directory+theme.Name+theme.DisplayName+theme.Version) {
			return errors.New("CSS theme name is missing")
		}
		if err := ValidateLogicalPath("theme/"+theme.Directory, maxPathLength); err != nil {
			return fmt.Errorf("CSS theme directory %q: %w", theme.Directory, err)
		}
		folded := strings.ToLower(theme.Directory)
		if previous, exists := seenThemes[folded]; exists {
			return fmt.Errorf("duplicate or case-colliding CSS theme directories %q and %q", previous, theme.Directory)
		}
		seenThemes[folded] = theme.Directory
	}
	seenArtwork := make(map[string]struct{}, len(m.Artwork))
	for _, artwork := range m.Artwork {
		if _, exists := seenArtwork[artwork.LogicalPath]; exists {
			return fmt.Errorf("duplicate artwork metadata for %q", artwork.LogicalPath)
		}
		seenArtwork[artwork.LogicalPath] = struct{}{}
		file, ok := fileByPath(m.Files, artwork.LogicalPath)
		if !ok {
			return fmt.Errorf("artwork path %q is not in the file inventory", artwork.LogicalPath)
		}
		if !validSHA256(artwork.SHA256) || artwork.SHA256 != file.SHA256 || artwork.Type == "" {
			return fmt.Errorf("artwork metadata does not match %q", artwork.LogicalPath)
		}
	}
	seenCompatibility := make(map[string]struct{}, len(m.Compatibility.UnverifiedBehaviors))
	for _, behavior := range m.Compatibility.UnverifiedBehaviors {
		if behavior == "" || containsControl(behavior) {
			return errors.New("compatibility behavior is empty or contains control characters")
		}
		if _, exists := seenCompatibility[behavior]; exists {
			return errors.New("duplicate compatibility behavior")
		}
		seenCompatibility[behavior] = struct{}{}
	}
	return nil
}

func ValidateLogicalPath(value string, maxLength int) error {
	if value == "" || value == "." {
		return errors.New("path is empty or ambiguous")
	}
	if !utf8.ValidString(value) {
		return errors.New("path is not valid UTF-8")
	}
	if !norm.NFC.IsNormalString(value) {
		return errors.New("path is not in Unicode NFC normalization form")
	}
	if len(value) > maxLength {
		return fmt.Errorf("path exceeds %d bytes", maxLength)
	}
	if strings.Contains(value, `\`) {
		return errors.New("backslashes are not allowed")
	}
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || windowsDrivePattern.MatchString(value) {
		return errors.New("absolute paths are not allowed")
	}
	for _, character := range value {
		if character == 0 || unicode.IsControl(character) {
			return errors.New("control characters are not allowed")
		}
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return errors.New("path is not normalized or contains traversal")
	}
	for _, segment := range strings.Split(value, "/") {
		if strings.Contains(segment, ":") {
			return errors.New("Windows alternate-data-stream separators are not allowed")
		}
		if strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
			return errors.New("path segments may not end with a dot or space")
		}
		upper := strings.ToUpper(segment)
		base, _, _ := strings.Cut(upper, ".")
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
			(len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return errors.New("Windows reserved device names are not allowed")
		}
	}
	return nil
}

func majorVersion(version string) string {
	major, _, _ := strings.Cut(version, ".")
	return major
}

func validSHA256(value string) bool {
	hash, err := hex.DecodeString(value)
	return err == nil && len(hash) == 32
}

func containsControl(value string) bool {
	for _, character := range value {
		if character == 0 || unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func fileByPath(files []File, logicalPath string) (File, bool) {
	for _, file := range files {
		if file.LogicalPath == logicalPath {
			return file, true
		}
	}
	return File{}, false
}
