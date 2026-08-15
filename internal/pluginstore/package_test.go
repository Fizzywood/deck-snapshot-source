package pluginstore

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type zipEntry struct {
	name string
	data string
	mode os.FileMode
}

func TestPreparePackageValidatesIdentityAndRemoteBinary(t *testing.T) {
	binary := []byte("verified remote binary")
	binaryHash := sha256.Sum256(binary)
	var packageBytes []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/package.zip":
			response.Write(packageBytes)
		case "/backend":
			response.Write(binary)
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	packageBytes = makeZIP(t, []zipEntry{
		{name: "fixture-plugin/plugin.json", data: `{"name":"Fixture Plugin","author":"Deck Snapshot Tests"}`},
		{name: "fixture-plugin/package.json", data: `{"name":"fixture-plugin","version":"1.2.3","remote_binary":[{"name":"backend","url":"` + server.URL + `/backend","sha256hash":"` + hex.EncodeToString(binaryHash[:]) + `"}]}`},
		{name: "fixture-plugin/dist/index.js", data: "console.log('fixture')"},
	})
	hash := sha256.Sum256(packageBytes)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := PreparePackage(context.Background(), testResolution(server.URL+"/package.zip", hex.EncodeToString(hash[:])), workspace, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Metadata.Version != "1.2.3" || prepared.Files != 4 {
		t.Fatalf("unexpected prepared package: %#v", prepared)
	}
	contents, err := os.ReadFile(filepath.Join(prepared.Root, "bin", "backend"))
	if err != nil || !bytes.Equal(contents, binary) {
		t.Fatalf("remote binary was not verified: %q %v", contents, err)
	}
}

func TestPreparePackageAcceptsVerifiedBundledBinary(t *testing.T) {
	archive := makeZIP(t, []zipEntry{
		{name: "fixture-plugin/plugin.json", data: `{"name":"Fixture Plugin","author":"Deck Snapshot Tests"}`},
		{name: "fixture-plugin/package.json", data: `{"name":"fixture-plugin","version":"1.2.3"}`},
		{name: "fixture-plugin/bin/backend", data: "bundled verified binary", mode: 0o755},
	})
	hash := sha256.Sum256(archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) { response.Write(archive) }))
	defer server.Close()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := PreparePackage(context.Background(), testResolution(server.URL, hex.EncodeToString(hash[:])), workspace, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(prepared.Root, "bin", "backend"))
	if err != nil || string(contents) != "bundled verified binary" {
		t.Fatalf("verified bundled binary was not preserved: %q %v", contents, err)
	}
}

func TestPreparePackageRejectsRemoteBinaryDestinationCollision(t *testing.T) {
	binary := []byte("verified remote binary")
	binaryHash := sha256.Sum256(binary)
	var archive []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/package.zip" {
			response.Write(archive)
			return
		}
		response.Write(binary)
	}))
	defer server.Close()
	archive = makeZIP(t, []zipEntry{
		{name: "fixture-plugin/plugin.json", data: `{"name":"Fixture Plugin","author":"Deck Snapshot Tests"}`},
		{name: "fixture-plugin/package.json", data: `{"name":"fixture-plugin","version":"1.2.3","remote_binary":[{"name":"backend","url":"` + server.URL + `/backend","sha256hash":"` + hex.EncodeToString(binaryHash[:]) + `"}]}`},
		{name: "fixture-plugin/bin/backend", data: "bundled collision", mode: 0o755},
	})
	hash := sha256.Sum256(archive)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := PreparePackage(context.Background(), testResolution(server.URL+"/package.zip", hex.EncodeToString(hash[:])), workspace, server.Client())
	if err == nil || !strings.Contains(err.Error(), "mismatched remote-binary destination") {
		t.Fatalf("remote-binary collision error = %v", err)
	}
}

func TestPreparePackageRejectsCaseCollidingRemoteBinaryDestination(t *testing.T) {
	binary := []byte("matching bundled remote binary")
	binaryHash := sha256.Sum256(binary)
	var archive []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/package.zip" {
			response.Write(archive)
			return
		}
		response.Write(binary)
	}))
	defer server.Close()
	archive = makeZIP(t, []zipEntry{
		{name: "fixture-plugin/plugin.json", data: `{"name":"Fixture Plugin","author":"Deck Snapshot Tests"}`},
		{name: "fixture-plugin/package.json", data: `{"name":"fixture-plugin","version":"1.2.3","remote_binary":[{"name":"backend","url":"` + server.URL + `/backend","sha256hash":"` + hex.EncodeToString(binaryHash[:]) + `"}]}`},
		{name: "fixture-plugin/bin/Backend", data: string(binary), mode: 0o755},
	})
	hash := sha256.Sum256(archive)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := PreparePackage(context.Background(), testResolution(server.URL+"/package.zip", hex.EncodeToString(hash[:])), workspace, server.Client())
	if err == nil || !strings.Contains(err.Error(), "case-colliding remote-binary destination") {
		t.Fatalf("case-colliding remote-binary error = %v", err)
	}
}

func TestPreparePackageReusesMatchingBundledRemoteBinary(t *testing.T) {
	binary := []byte("matching bundled remote binary")
	binaryHash := sha256.Sum256(binary)
	var archive []byte
	var remoteRequests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/package.zip" {
			response.Write(archive)
			return
		}
		remoteRequests.Add(1)
		response.Write(binary)
	}))
	defer server.Close()
	archive = makeZIP(t, []zipEntry{
		{name: "fixture-plugin/plugin.json", data: `{"name":"Fixture Plugin","author":"Deck Snapshot Tests"}`},
		{name: "fixture-plugin/package.json", data: `{"name":"fixture-plugin","version":"1.2.3","remote_binary":[{"name":"backend","url":"` + server.URL + `/backend","sha256hash":"` + hex.EncodeToString(binaryHash[:]) + `"}]}`},
		{name: "fixture-plugin/bin/backend", data: string(binary), mode: 0o755},
	})
	hash := sha256.Sum256(archive)
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := PreparePackage(context.Background(), testResolution(server.URL+"/package.zip", hex.EncodeToString(hash[:])), workspace, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if remoteRequests.Load() != 0 {
		t.Fatalf("matching bundled remote binary triggered %d download requests", remoteRequests.Load())
	}
	contents, err := os.ReadFile(filepath.Join(prepared.Root, "bin", "backend"))
	if err != nil || !bytes.Equal(contents, binary) {
		t.Fatalf("matching bundled remote binary changed: %q %v", contents, err)
	}
}

func TestPreparePackageRejectsHostileZIPs(t *testing.T) {
	cases := map[string][]zipEntry{
		"traversal": {{name: "fixture-plugin/../outside", data: "bad"}},
		"ADS":       {{name: "fixture-plugin/file:stream", data: "bad"}},
		"reserved":  {{name: "fixture-plugin/NUL.txt", data: "bad"}},
		"trailing":  {{name: "fixture-plugin/trailing.", data: "bad"}},
		"two roots": {
			{name: "fixture-plugin/plugin.json", data: `{}`},
			{name: "other/package.json", data: `{}`},
		},
		"symlink":      {{name: "fixture-plugin/link", data: "target", mode: os.ModeSymlink | 0o777}},
		"special file": {{name: "fixture-plugin/pipe", data: "", mode: os.ModeNamedPipe | 0o600}},
		"identity mismatch": {
			{name: "fixture-plugin/plugin.json", data: `{"name":"Other","author":"Deck Snapshot Tests"}`},
			{name: "fixture-plugin/package.json", data: `{"version":"1.2.3"}`},
		},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			archive := makeZIP(t, entries)
			hash := sha256.Sum256(archive)
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) { response.Write(archive) }))
			defer server.Close()
			workspace := filepath.Join(t.TempDir(), "workspace")
			os.Mkdir(workspace, 0o700)
			if _, err := PreparePackage(context.Background(), testResolution(server.URL, hex.EncodeToString(hash[:])), workspace, server.Client()); err == nil {
				t.Fatal("hostile ZIP was accepted")
			}
		})
	}
}

func TestPreparePackageHonorsCancellation(t *testing.T) {
	archive := makeZIP(t, []zipEntry{
		{name: "fixture-plugin/plugin.json", data: `{"name":"Fixture Plugin","author":"Deck Snapshot Tests"}`},
		{name: "fixture-plugin/package.json", data: `{"version":"1.2.3"}`},
	})
	hash := sha256.Sum256(archive)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) { response.Write(archive) }))
	defer server.Close()
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PreparePackage(ctx, testResolution(server.URL, hex.EncodeToString(hash[:])), workspace, server.Client()); err == nil {
		t.Fatal("PreparePackage accepted a cancelled operation")
	}
}

func TestValidArtifactURLRejectsCredentialsQueriesAndFragments(t *testing.T) {
	unsafe := []string{
		"http://example.com/package.zip",
		"https://user:secret@example.com/package.zip",
		"https://example.com/package.zip?token=secret",
		"https://example.com/package.zip#fragment",
	}
	for _, value := range unsafe {
		if ValidArtifactURL(value) {
			t.Fatalf("unsafe artifact URL accepted: %q", value)
		}
	}
}

func TestDownloadRedirectURLAllowsOnlyGitHubReleaseAssetQueries(t *testing.T) {
	if !validDownloadRedirectURL("https://release-assets.githubusercontent.com/github-production-release-asset/file?sig=temporary") {
		t.Fatal("GitHub release asset signed redirect was rejected")
	}
	unsafe := []string{
		"https://example.com/file?sig=temporary",
		"https://user:secret@release-assets.githubusercontent.com/file?sig=temporary",
		"http://release-assets.githubusercontent.com/file?sig=temporary",
		"https://release-assets.githubusercontent.com/file?sig=temporary#fragment",
	}
	for _, value := range unsafe {
		if validDownloadRedirectURL(value) {
			t.Fatalf("unsafe redirect URL accepted: %s", value)
		}
	}
}

func TestDownloadRedirectErrorRedactsSignedURL(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, "https://example.com/file?temporary_secret=do-not-log", http.StatusFound)
	}))
	defer server.Close()
	err := downloadVerified(context.Background(), server.Client(), server.URL, strings.Repeat("0", 64), filepath.Join(t.TempDir(), "download"), 1024, 0o600)
	if err == nil || strings.Contains(err.Error(), "temporary_secret") || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("redirect error was not safely redacted: %v", err)
	}
}

func TestRemoteBinaryRejectsWindowsDangerousNames(t *testing.T) {
	for _, name := range []string{"file:stream", "NUL", "COM1.exe", "trailing."} {
		binary := RemoteBinary{Name: name, URL: "https://example.test/binary", SHA256Hash: strings.Repeat("a", 64)}
		if err := validateRemoteBinary(binary); err == nil {
			t.Fatalf("unsafe remote binary name accepted: %q", name)
		}
	}
}

func TestPreparePackageRejectsChecksumMismatch(t *testing.T) {
	archive := makeZIP(t, []zipEntry{{name: "fixture-plugin/plugin.json", data: `{}`}})
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) { response.Write(archive) }))
	defer server.Close()
	workspace := filepath.Join(t.TempDir(), "workspace")
	os.Mkdir(workspace, 0o700)
	if _, err := PreparePackage(context.Background(), testResolution(server.URL, strings.Repeat("0", 64)), workspace, server.Client()); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
}

func makeZIP(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		output, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testResolution(url, hash string) Resolution {
	return Resolution{
		SnapshotDirectory: "fixture-plugin",
		SnapshotName:      "Fixture Plugin",
		SnapshotAuthor:    "Deck Snapshot Tests",
		SnapshotVersion:   "1.2.3",
		Status:            "resolved",
		StoreID:           1,
		StoreName:         "Fixture Plugin",
		StoreAuthor:       "Deck Snapshot Tests",
		ResolvedVersion:   "1.2.3",
		SHA256:            hash,
		ArtifactURL:       url,
	}
}
