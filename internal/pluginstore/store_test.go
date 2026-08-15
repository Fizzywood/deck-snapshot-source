package pluginstore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Fizzywood/deck-snapshot/internal/manifest"
)

func TestResolveCatalogUsesFirstExactStableVersion(t *testing.T) {
	catalog := []Plugin{{
		ID: 7, Name: "CSS Loader", Author: "DeckThemes", Visible: true,
		Versions: []Version{{Name: "2.1.2", Hash: strings.Repeat("a", 64)}, {Name: "2.1.1", Hash: strings.Repeat("b", 64)}},
	}}
	resolved := ResolveCatalog(catalog, []manifest.Plugin{{Directory: "SDH-CssLoader", Name: "CSS Loader", Author: "DeckThemes", Version: "2.0.0"}})
	if len(resolved) != 1 || resolved[0].Blocking || resolved[0].ResolvedVersion != "2.1.2" || resolved[0].ArtifactURL != OfficialCDNBase+strings.Repeat("a", 64)+".zip" {
		t.Fatalf("ResolveCatalog() = %#v", resolved)
	}
}

func TestResolveCatalogFailsClosedOnIdentityProblems(t *testing.T) {
	catalog := []Plugin{
		{ID: 1, Name: "Plugin", Author: "Other", Visible: true, Versions: []Version{{Name: "1", Hash: strings.Repeat("a", 64)}}},
		{ID: 2, Name: "Ambiguous", Author: "Author", Visible: true, Versions: []Version{{Name: "1", Hash: strings.Repeat("b", 64)}}},
		{ID: 3, Name: "Ambiguous", Author: "Author", Visible: true, Versions: []Version{{Name: "1", Hash: strings.Repeat("c", 64)}}},
	}
	resolved := ResolveCatalog(catalog, []manifest.Plugin{{Name: "Plugin", Author: "Expected"}, {Name: "Ambiguous", Author: "Author"}, {Name: "Missing", Author: "Author"}})
	statuses := []string{resolved[0].Status, resolved[1].Status, resolved[2].Status}
	if strings.Join(statuses, ",") != "author_mismatch,ambiguous,missing" {
		t.Fatalf("unexpected statuses: %v", statuses)
	}
	for _, resolution := range resolved {
		if !resolution.Blocking {
			t.Fatalf("identity problem did not block: %#v", resolution)
		}
	}
}

func TestResolveCatalogRejectsUnsafeArtifact(t *testing.T) {
	artifact := "http://example.com/plugin.zip"
	catalog := []Plugin{{ID: 1, Name: "Plugin", Author: "Author", Visible: true, Versions: []Version{{Name: "1", Hash: strings.Repeat("a", 64), Artifact: &artifact}}}}
	resolved := ResolveCatalog(catalog, []manifest.Plugin{{Name: "Plugin", Author: "Author"}})
	if resolved[0].Status != "invalid_artifact_url" || !resolved[0].Blocking {
		t.Fatalf("unsafe artifact was accepted: %#v", resolved[0])
	}
}

func TestClientFetchBoundsAndDecodesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.Write([]byte(`[{"id":7,"name":"CSS Loader","author":"DeckThemes","visible":true,"versions":[{"name":"2.1.2","hash":"` + strings.Repeat("a", 64) + `"}]}]`))
	}))
	defer server.Close()
	client := &Client{StoreURL: server.URL, HTTPClient: server.Client()}
	catalog, err := client.Fetch(context.Background())
	if err != nil || len(catalog) != 1 || catalog[0].ID != 7 {
		t.Fatalf("Fetch() catalog=%#v error=%v", catalog, err)
	}
}

func TestClientFetchRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	client := &Client{StoreURL: server.URL, HTTPClient: server.Client()}
	if _, err := client.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch() accepted an HTTP error")
	}
}
