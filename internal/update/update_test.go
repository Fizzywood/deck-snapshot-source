package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestCheckAcceptsOnlyNewerStableFixedRelease(t *testing.T) {
	installer := validInstaller(t)
	manifest := testManifest("v0.1.6", installer)
	client := testClient(manifest, installer, nil)
	status, err := client.Check(context.Background(), "v0.1.5")
	if err != nil || status.Available != "v0.1.6" || status.UpToDate {
		t.Fatalf("Check(newer) = %#v, %v", status, err)
	}
	for _, version := range []string{"v0.1.6", "v0.1.7"} {
		if _, err := client.Install(context.Background(), version); err == nil {
			t.Fatalf("Install(%q) accepted a same-version or downgrade target", version)
		}
	}
}

func TestCheckRejectsMalformedPrereleaseAndUnexpectedAssets(t *testing.T) {
	installer := validInstaller(t)
	for _, manifest := range []string{
		"{",
		strings.Replace(testManifest("v0.1.6", installer), "v0.1.6", "v0.1.6-rc.1", 1),
		strings.Replace(testManifest("v0.1.6", installer), "deck_snapshot_installer.desktop", "unexpected.desktop", 1),
		strings.Replace(testManifest("v0.1.6", installer), "https://github.com/Fizzywood", "https://example.invalid/Fizzywood", 1),
	} {
		if _, err := testClient(manifest, installer, nil).Check(context.Background(), "v0.1.5"); err == nil {
			t.Fatalf("Check accepted invalid manifest %q", manifest)
		}
	}
}

func TestInstallVerifiesBytesBeforeRunning(t *testing.T) {
	installer := validInstaller(t)
	manifest := testManifest("v0.1.6", installer)
	ran := false
	client := testClient(manifest, installer, func(_ context.Context, path string) error {
		ran = true
		contents, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(contents, installer) {
			return fmt.Errorf("verified installer was not private exact bytes")
		}
		return nil
	})
	if _, err := client.Install(context.Background(), "v0.1.5"); err != nil || !ran {
		t.Fatalf("Install() = %v, ran=%t", err, ran)
	}
	ran = false
	if _, err := testClient(manifest, []byte("modified"), func(context.Context, string) error { ran = true; return nil }).Install(context.Background(), "v0.1.5"); err == nil || ran {
		t.Fatalf("Install ran before checksum verification: err=%v ran=%t", err, ran)
	}
}

func TestCheckBoundsResponsesAndRejectsUnsafeRedirect(t *testing.T) {
	client := New(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return response(request, http.StatusOK, strings.Repeat("x", maxManifestBytes+1)), nil
	})})
	if _, err := client.Check(context.Background(), "v0.1.5"); err == nil {
		t.Fatal("Check accepted oversized stable metadata")
	}
	client = New(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://example.invalid/installer"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})})
	if _, err := client.Check(context.Background(), "v0.1.5"); err == nil {
		t.Fatal("Check accepted redirect outside approved GitHub release hosts")
	}
}

func testClient(manifest string, installer []byte, run func(context.Context, string) error) Client {
	return Client{HTTPClient: &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.String() {
		case stableManifestURL:
			return response(request, http.StatusOK, manifest), nil
		case "https://github.com/Fizzywood/deck-snapshot-releases/releases/download/v0.1.6/deck_snapshot_installer.desktop":
			return response(request, http.StatusOK, string(installer)), nil
		default:
			return response(request, http.StatusNotFound, ""), nil
		}
	})}, manifestURL: stableManifestURL, Run: run}
}

func testManifest(version string, installer []byte) string {
	sum := sha256.Sum256(installer)
	asset := func(name string, size int, digest string) string {
		return fmt.Sprintf(`{"name":%q,"url":%q,"size":%d,"sha256":%q}`, name, "https://github.com/Fizzywood/deck-snapshot-releases/releases/download/"+version+"/"+name, size, digest)
	}
	return fmt.Sprintf(`{"schema_version":1,"version":%q,"published_at":"2026-08-16T00:00:00Z","assets":[%s,%s,%s]}`,
		version,
		asset(installerName, len(installer), fmt.Sprintf("%x", sum)),
		asset("deck-snapshot-linux-amd64.tar.gz", 1, strings.Repeat("a", 64)),
		asset("deck-snapshot-linux-amd64.sha256", 1, strings.Repeat("b", 64)))
}

func validInstaller(t *testing.T) []byte {
	t.Helper()
	payload := base64.StdEncoding.EncodeToString([]byte("#!/usr/bin/env bash\nexit 0\n"))
	return []byte("[Desktop Entry]\nExec=/usr/bin/env bash -c \"echo " + payload + " | base64 -d | bash\"\n")
}

func response(request *http.Request, status int, value string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(value)), Request: request}
}
