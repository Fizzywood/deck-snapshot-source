// Package update implements the deliberately small, user-initiated release
// update boundary. It accepts only the project's fixed public release channel.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	legacyReleaseOwner    = "Fizzywood"
	migrationReleaseOwner = "TAndrson"
	releaseRepository     = "deck-snapshot-releases"
	stableManifestURL     = "https://github.com/Fizzywood/deck-snapshot-releases/releases/latest/download/stable.json"
	installerName         = "deck_snapshot_installer.desktop"
	maxManifestBytes      = 64 * 1024
	maxInstallerBytes     = 4 * 1024 * 1024
)

var stableVersion = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)
var sha256Value = regexp.MustCompile(`^[0-9a-f]{64}$`)
var installerExec = regexp.MustCompile(`^Exec=/usr/bin/env bash -c "echo ([A-Za-z0-9+/=]+) \| base64 -d \| bash"$`)

type Manifest struct {
	SchemaVersion int     `json:"schema_version"`
	Version       string  `json:"version"`
	PublishedAt   string  `json:"published_at"`
	Assets        []Asset `json:"assets"`
}

type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Status struct {
	Installed string   `json:"installed"`
	Available string   `json:"available"`
	UpToDate  bool     `json:"up_to_date"`
	Asset     Asset    `json:"-"`
	Manifest  Manifest `json:"-"`
}

type Client struct {
	HTTPClient  *http.Client
	manifestURL string
	Run         func(context.Context, string) error
}

func New(httpClient *http.Client) Client {
	return Client{HTTPClient: httpClient, manifestURL: stableManifestURL, Run: runInstaller}
}

func (client Client) Check(ctx context.Context, installed string) (Status, error) {
	current, err := parseVersion(installed)
	if err != nil {
		return Status{}, fmt.Errorf("installed version is not a supported stable release: %w", err)
	}
	manifest, err := client.fetchManifest(ctx)
	if err != nil {
		return Status{}, err
	}
	available, err := parseVersion(manifest.Version)
	if err != nil {
		return Status{}, fmt.Errorf("release manifest has an unsupported target version: %w", err)
	}
	if compareVersions(available, current) < 0 {
		return Status{}, errors.New("release manifest would downgrade the installed version")
	}
	asset, err := manifest.installer()
	if err != nil {
		return Status{}, err
	}
	return Status{Installed: installed, Available: manifest.Version, UpToDate: compareVersions(available, current) == 0, Asset: asset, Manifest: manifest}, nil
}

// Install performs a fresh fixed-channel check and starts the existing
// checksum-bound installer only after its exact expected installer bytes have
// been downloaded and hashed. A same-version or older target is refused.
func (client Client) Install(ctx context.Context, installed string) (Status, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	status, err := client.Check(ctx, installed)
	if err != nil {
		return Status{}, err
	}
	if status.UpToDate {
		return Status{}, errors.New("the installed version is already current")
	}
	installer, cleanup, err := client.downloadInstaller(ctx, status.Asset)
	if err != nil {
		return Status{}, err
	}
	defer cleanup()
	run := client.Run
	if run == nil {
		run = runInstaller
	}
	if err := run(ctx, installer); err != nil {
		return Status{}, fmt.Errorf("run verified installer: %w", err)
	}
	return status, nil
}

func (client Client) fetchManifest(ctx context.Context) (Manifest, error) {
	body, err := client.get(ctx, client.manifestURL, maxManifestBytes, true)
	if err != nil {
		return Manifest{}, fmt.Errorf("fetch stable release metadata: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, errors.New("stable release metadata is malformed")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("stable release metadata has trailing data")
	}
	if err := manifest.validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func (client Client) downloadInstaller(ctx context.Context, asset Asset) (string, func(), error) {
	if asset.Name != installerName || asset.Size < 1 || asset.Size > maxInstallerBytes || !sha256Value.MatchString(asset.SHA256) || !validAssetURL(asset.URL, "") {
		return "", nil, errors.New("release manifest has an invalid installer asset")
	}
	body, err := client.get(ctx, asset.URL, asset.Size, false)
	if err != nil {
		return "", nil, fmt.Errorf("download verified installer: %w", err)
	}
	if int64(len(body)) != asset.Size {
		return "", nil, errors.New("installer download size does not match release metadata")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != asset.SHA256 {
		return "", nil, errors.New("installer download checksum does not match release metadata")
	}
	directory, err := os.MkdirTemp("", "deck-snapshot-update-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	installerPath := filepath.Join(directory, installerName)
	if err := os.WriteFile(installerPath, body, 0o700); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.Chmod(installerPath, 0o700); err != nil {
		cleanup()
		return "", nil, err
	}
	return installerPath, cleanup, nil
}

func (client Client) get(ctx context.Context, rawURL string, maximum int64, manifest bool) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if maximum < 1 {
		return nil, errors.New("download size limit is invalid")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (manifest && rawURL != client.manifestURL) || (!manifest && !validAssetURL(rawURL, "")) {
		return nil, errors.New("release URL is not allowed")
	}
	if parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.Port() != "" {
		return nil, errors.New("release URL is not an allowed GitHub HTTPS location")
	}
	base := client.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	httpClient := *base
	if httpClient.Timeout <= 0 || httpClient.Timeout > 30*time.Second {
		httpClient.Timeout = 30 * time.Second
	}
	httpClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 5 || !allowedRedirectURL(request.URL) {
			return errors.New("release download redirect is not allowed")
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Request == nil || !allowedRedirectURL(response.Request.URL) {
		return nil, errors.New("release download response is not allowed")
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > maximum {
		return nil, errors.New("release download exceeded its size limit")
	}
	return contents, nil
}

func (manifest Manifest) validate() error {
	if manifest.SchemaVersion != 1 {
		return errors.New("stable release metadata has an unsupported schema")
	}
	if _, err := parseVersion(manifest.Version); err != nil {
		return errors.New("stable release metadata has an unsupported version")
	}
	if timestamp, err := time.Parse(time.RFC3339, manifest.PublishedAt); err != nil || timestamp.IsZero() {
		return errors.New("stable release metadata has an invalid publication time")
	}
	if len(manifest.Assets) != 3 {
		return errors.New("stable release metadata has an unexpected asset count")
	}
	seen := map[string]bool{}
	for _, asset := range manifest.Assets {
		if seen[asset.Name] || asset.Size < 1 || asset.Size > 512*1024*1024 || !sha256Value.MatchString(asset.SHA256) || !validAssetURL(asset.URL, manifest.Version) {
			return errors.New("stable release metadata has an invalid asset")
		}
		seen[asset.Name] = true
	}
	for _, name := range []string{installerName, "deck-snapshot-linux-amd64.tar.gz", "deck-snapshot-linux-amd64.sha256"} {
		if !seen[name] {
			return errors.New("stable release metadata is missing a required asset")
		}
	}
	return nil
}

func (manifest Manifest) installer() (Asset, error) {
	for _, asset := range manifest.Assets {
		if asset.Name == installerName {
			return asset, nil
		}
	}
	return Asset{}, errors.New("stable release metadata is missing the installer")
}

func allowedReleaseOwner(owner string) bool {
	return owner == legacyReleaseOwner || owner == migrationReleaseOwner
}

func allowedReleaseAssetName(name string) bool {
	return name == installerName || name == "deck-snapshot-linux-amd64.tar.gz" || name == "deck-snapshot-linux-amd64.sha256"
}

func validAssetURL(rawURL, version string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path.Clean(parsed.Path), "/"), "/")
	if len(parts) != 6 || !allowedReleaseOwner(parts[0]) || parts[1] != releaseRepository || parts[2] != "releases" || parts[3] != "download" || parts[4] == "" || !allowedReleaseAssetName(parts[5]) {
		return false
	}
	return version == "" || parts[4] == version
}

func allowedRedirectURL(value *url.URL) bool {
	if value == nil || value.Scheme != "https" || value.User != nil || value.Port() != "" || value.RawQuery != "" || value.Fragment != "" {
		return false
	}
	host := strings.ToLower(value.Hostname())
	if host == "github.com" {
		parts := strings.Split(strings.TrimPrefix(path.Clean(value.Path), "/"), "/")
		if len(parts) != 6 || !allowedReleaseOwner(parts[0]) || parts[1] != releaseRepository || parts[2] != "releases" {
			return false
		}
		if parts[3] == "latest" {
			return parts[4] == "download" && parts[5] == "stable.json"
		}
		return parts[3] == "download" && parts[4] != "" && allowedReleaseAssetName(parts[5])
	}
	return host == "release-assets.githubusercontent.com" || host == "objects.githubusercontent.com" || host == "github-releases.githubusercontent.com"
}

type version struct{ major, minor, patch int }

func parseVersion(value string) (version, error) {
	matches := stableVersion.FindStringSubmatch(value)
	if matches == nil {
		return version{}, errors.New("not a stable vX.Y.Z version")
	}
	values := [3]int{}
	for index := range values {
		parsed, err := strconv.Atoi(matches[index+1])
		if err != nil || parsed < 0 {
			return version{}, errors.New("invalid version component")
		}
		values[index] = parsed
	}
	return version{values[0], values[1], values[2]}, nil
}

func compareVersions(left, right version) int {
	for _, values := range [][2]int{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if values[0] < values[1] {
			return -1
		}
		if values[0] > values[1] {
			return 1
		}
	}
	return 0
}

func runInstaller(ctx context.Context, installer string) error {
	info, err := os.Lstat(installer)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > maxInstallerBytes {
		return errors.New("verified installer path is unsafe")
	}
	contents, err := os.ReadFile(installer)
	if err != nil {
		return err
	}
	var execLine string
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.HasPrefix(line, "Exec=") {
			if execLine != "" {
				return errors.New("verified installer has multiple execution entries")
			}
			execLine = strings.TrimSuffix(line, "\r")
		}
	}
	matches := installerExec.FindStringSubmatch(execLine)
	if matches == nil {
		return errors.New("verified installer has an unexpected execution entry")
	}
	payload, err := base64.StdEncoding.DecodeString(matches[1])
	if err != nil || len(payload) == 0 || len(payload) > maxInstallerBytes {
		return errors.New("verified installer payload is invalid")
	}
	directory, err := os.MkdirTemp("", "deck-snapshot-update-bootstrap-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	bootstrap := filepath.Join(directory, "bootstrap-installer.sh")
	if err := os.WriteFile(bootstrap, payload, 0o700); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "/usr/bin/env", "bash", bootstrap)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return err
	}
	return nil
}
