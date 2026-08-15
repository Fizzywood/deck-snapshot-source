// Package pluginstore resolves current stable Decky packages from verified store metadata.
package pluginstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/manifest"
)

const (
	OfficialStoreURL = "https://plugins.deckbrew.xyz/plugins"
	OfficialCDNBase  = "https://cdn.tzatzikiweeb.moe/file/steam-deck-homebrew/versions/"
	maxCatalogBytes  = 16 << 20
)

type Version struct {
	Name     string  `json:"name"`
	Hash     string  `json:"hash"`
	Artifact *string `json:"artifact"`
}

type Plugin struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	Author   string    `json:"author"`
	Versions []Version `json:"versions"`
	Visible  bool      `json:"visible"`
}

type Resolution struct {
	SnapshotDirectory string `json:"snapshot_directory"`
	SnapshotName      string `json:"snapshot_name"`
	SnapshotAuthor    string `json:"snapshot_author,omitempty"`
	SnapshotVersion   string `json:"snapshot_version,omitempty"`
	Status            string `json:"status"`
	Message           string `json:"message"`
	StoreID           int64  `json:"store_id,omitempty"`
	StoreName         string `json:"store_name,omitempty"`
	StoreAuthor       string `json:"store_author,omitempty"`
	ResolvedVersion   string `json:"resolved_version,omitempty"`
	SHA256            string `json:"sha256,omitempty"`
	ArtifactURL       string `json:"artifact_url,omitempty"`
	Blocking          bool   `json:"blocking"`
}

type Resolver interface {
	Resolve(context.Context, []manifest.Plugin) ([]Resolution, error)
}

type Client struct {
	StoreURL   string
	HTTPClient *http.Client
}

func NewOfficial() *Client {
	return &Client{
		StoreURL: OfficialStoreURL,
		HTTPClient: &http.Client{
			Timeout: 20 * time.Second,
			CheckRedirect: func(request *http.Request, previous []*http.Request) error {
				if request == nil || request.URL == nil || request.URL.Scheme != "https" || request.URL.Hostname() != "plugins.deckbrew.xyz" || request.URL.User != nil || request.URL.RawQuery != "" || request.URL.Fragment != "" {
					return errors.New("official plugin store redirect is unsafe")
				}
				if len(previous) >= 10 {
					return errors.New("official plugin store exceeded the redirect limit")
				}
				return nil
			},
		},
	}
}

func (client *Client) Resolve(ctx context.Context, snapshots []manifest.Plugin) ([]Resolution, error) {
	catalog, err := client.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	return ResolveCatalog(catalog, snapshots), nil
}

func (client *Client) Fetch(ctx context.Context) ([]Plugin, error) {
	if client == nil || client.HTTPClient == nil || client.StoreURL == "" {
		return nil, errors.New("plugin store client is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.StoreURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create plugin store request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Deck-Snapshot/0.1")
	response, err := client.HTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch official plugin store: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official plugin store returned HTTP %d", response.StatusCode)
	}
	if client.StoreURL == OfficialStoreURL && (response.Request == nil || response.Request.URL == nil || response.Request.URL.Scheme != "https" || response.Request.URL.Hostname() != "plugins.deckbrew.xyz") {
		return nil, errors.New("official plugin store resolved to an unexpected source")
	}
	limited := io.LimitReader(response.Body, maxCatalogBytes+1)
	contents, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read official plugin store: %w", err)
	}
	if len(contents) > maxCatalogBytes {
		return nil, errors.New("official plugin store response exceeded the size limit")
	}
	var catalog []Plugin
	if err := json.Unmarshal(contents, &catalog); err != nil {
		return nil, fmt.Errorf("decode official plugin store: %w", err)
	}
	return catalog, nil
}

func ResolveCatalog(catalog []Plugin, snapshots []manifest.Plugin) []Resolution {
	result := make([]Resolution, 0, len(snapshots))
	for _, snapshotPlugin := range snapshots {
		base := Resolution{
			SnapshotDirectory: snapshotPlugin.Directory,
			SnapshotName:      snapshotPlugin.Name,
			SnapshotAuthor:    snapshotPlugin.Author,
			SnapshotVersion:   snapshotPlugin.Version,
			Blocking:          true,
		}
		var nameMatches []Plugin
		var exact []Plugin
		for _, storePlugin := range catalog {
			if !storePlugin.Visible || storePlugin.Name != snapshotPlugin.Name {
				continue
			}
			nameMatches = append(nameMatches, storePlugin)
			if snapshotPlugin.Author != "" && storePlugin.Author == snapshotPlugin.Author {
				exact = append(exact, storePlugin)
			}
		}
		if len(exact) == 0 {
			if len(nameMatches) == 0 {
				base.Status = "missing"
				base.Message = "No visible official stable plugin matched the snapshot name and author."
			} else {
				base.Status = "author_mismatch"
				base.Message = "The official store name matched, but the author identity did not."
			}
			result = append(result, base)
			continue
		}
		if len(exact) != 1 {
			base.Status = "ambiguous"
			base.Message = "More than one official store entry matched the snapshot identity."
			result = append(result, base)
			continue
		}
		storePlugin := exact[0]
		if len(storePlugin.Versions) == 0 {
			base.Status = "no_stable_version"
			base.Message = "The matching official plugin has no stable version."
			result = append(result, base)
			continue
		}
		version := storePlugin.Versions[0]
		if !validSHA256(version.Hash) || version.Name == "" {
			base.Status = "invalid_store_metadata"
			base.Message = "The matching official stable version has invalid integrity metadata."
			result = append(result, base)
			continue
		}
		artifact := OfficialCDNBase + version.Hash + ".zip"
		if version.Artifact != nil && *version.Artifact != "" {
			artifact = *version.Artifact
		}
		if !ValidArtifactURL(artifact) {
			base.Status = "invalid_artifact_url"
			base.Message = "The official stable version has a non-HTTPS or credential-bearing artifact URL."
			result = append(result, base)
			continue
		}
		base.Status = "resolved"
		base.Message = "Resolved the unique current official stable plugin identity."
		base.StoreID = storePlugin.ID
		base.StoreName = storePlugin.Name
		base.StoreAuthor = storePlugin.Author
		base.ResolvedVersion = version.Name
		base.SHA256 = strings.ToLower(version.Hash)
		base.ArtifactURL = artifact
		base.Blocking = false
		result = append(result, base)
	}
	return result
}

func CatalogFingerprint(catalog []Plugin) (string, error) {
	contents, err := json.Marshal(catalog)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(contents)
	return hex.EncodeToString(hash[:]), nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

// ValidArtifactURL accepts only credential-free HTTPS download URLs.
func ValidArtifactURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == "" && parsed.RawQuery == ""
}
