package deckyapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const testToken = "01234567-89ab-cdef-0123-456789abcdef"

type receivedCall struct {
	Type  int               `json:"type"`
	ID    int               `json:"id"`
	Route string            `json:"route"`
	Args  []json.RawMessage `json:"args"`
}

func TestProbeValidatesVersionWithoutOpeningWebsocket(t *testing.T) {
	t.Parallel()
	deckyHome := writeDeckyVersion(t, SupportedVersion)
	var tokenRequests atomic.Int32
	var websocketRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/auth/token":
			tokenRequests.Add(1)
			_, _ = response.Write([]byte(testToken))
		case "/ws":
			websocketRequests.Add(1)
			http.Error(response, "unexpected websocket", http.StatusBadRequest)
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	client, err := newTestClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(context.Background(), deckyHome); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
	if tokenRequests.Load() != 1 || websocketRequests.Load() != 0 {
		t.Fatalf("requests = token %d, websocket %d", tokenRequests.Load(), websocketRequests.Load())
	}
}

func TestProbeRejectsUnsupportedVersionBeforeNetwork(t *testing.T) {
	t.Parallel()
	deckyHome := writeDeckyVersion(t, "v3.2.7")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.Error(response, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client, err := newTestClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(context.Background(), deckyHome); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Probe() error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("network requests = %d, want 0", requests.Load())
	}
}

func TestInstallUsesExactPromptAndCompletionProtocol(t *testing.T) {
	t.Parallel()
	archive, checksum := writeArchive(t)
	serverErrors := make(chan error, 1)
	server := newProtocolServer(t, func(ctx context.Context, connection *websocket.Conn) error {
		first, err := readCall(ctx, connection)
		if err != nil {
			return err
		}
		if first.Type != 0 || first.ID != 1 || first.Route != "utilities/install_plugin" || len(first.Args) != 5 {
			return &protocolTestError{"unexpected install request"}
		}
		var artifact, name, version, receivedHash string
		var installType int
		if json.Unmarshal(first.Args[0], &artifact) != nil || json.Unmarshal(first.Args[1], &name) != nil || json.Unmarshal(first.Args[2], &version) != nil || json.Unmarshal(first.Args[3], &receivedHash) != nil || json.Unmarshal(first.Args[4], &installType) != nil {
			return &protocolTestError{"invalid install arguments"}
		}
		if !strings.HasPrefix(artifact, "file://") || name != "Fixture Plugin" || version != "1.2.3" || receivedHash != checksum || installType != 4 {
			return &protocolTestError{"install arguments did not match"}
		}
		if err := wsjson.Write(ctx, connection, inboundMessage{Type: 3, Event: "loader/add_plugin_install_prompt", Args: rawArguments("Fixture Plugin", "1.2.3", "42.7", checksum, 4)}); err != nil {
			return err
		}
		if err := wsjson.Write(ctx, connection, inboundMessage{Type: 1, ID: 1, Result: json.RawMessage("null")}); err != nil {
			return err
		}
		second, err := readCall(ctx, connection)
		if err != nil {
			return err
		}
		var requestID string
		if second.Type != 0 || second.ID != 2 || second.Route != "utilities/confirm_plugin_install" || len(second.Args) != 1 || json.Unmarshal(second.Args[0], &requestID) != nil || requestID != "42.7" {
			return &protocolTestError{"unexpected confirmation request"}
		}
		if err := wsjson.Write(ctx, connection, inboundMessage{Type: 3, Event: "loader/plugin_download_finish", Args: rawArguments("Fixture Plugin")}); err != nil {
			return err
		}
		return wsjson.Write(ctx, connection, inboundMessage{Type: 1, ID: 2, Result: json.RawMessage("null")})
	}, serverErrors)
	t.Cleanup(server.Close)
	client, err := newTestClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	request := InstallRequest{ArchivePath: archive, Name: "Fixture Plugin", Version: "1.2.3", SHA256: checksum, Replace: true}
	if err := client.Install(context.Background(), request); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	select {
	case err := <-serverErrors:
		if err != nil {
			t.Fatal(err)
		}
	default:
	}
}

func TestInstallRejectsChecksumMismatchBeforeNetwork(t *testing.T) {
	t.Parallel()
	archive, _ := writeArchive(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.Error(response, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client, err := newTestClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.Install(context.Background(), InstallRequest{ArchivePath: archive, Name: "Fixture Plugin", Version: "1.2.3", SHA256: strings.Repeat("0", 64)})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Install() error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("network requests = %d, want 0", requests.Load())
	}
}

func TestUninstallUsesBoundedRoute(t *testing.T) {
	t.Parallel()
	serverErrors := make(chan error, 1)
	server := newProtocolServer(t, func(ctx context.Context, connection *websocket.Conn) error {
		call, err := readCall(ctx, connection)
		if err != nil {
			return err
		}
		var directory string
		if call.Type != 0 || call.ID != 1 || call.Route != "utilities/uninstall_plugin" || len(call.Args) != 1 || json.Unmarshal(call.Args[0], &directory) != nil || directory != "fixture-plugin" {
			return &protocolTestError{"unexpected uninstall request"}
		}
		return wsjson.Write(ctx, connection, inboundMessage{Type: 1, ID: 1, Result: json.RawMessage("null")})
	}, serverErrors)
	t.Cleanup(server.Close)
	client, err := newTestClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Uninstall(context.Background(), "fixture-plugin"); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	select {
	case err := <-serverErrors:
		if err != nil {
			t.Fatal(err)
		}
	default:
	}
}

func newProtocolServer(t *testing.T, serve func(context.Context, *websocket.Conn) error, errorsChannel chan<- error) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/auth/token":
			_, _ = response.Write([]byte(testToken))
		case "/ws":
			if request.URL.Query().Get("auth") != testToken {
				http.Error(response, "unauthorized", http.StatusUnauthorized)
				return
			}
			connection, err := websocket.Accept(response, request, nil)
			if err != nil {
				errorsChannel <- err
				return
			}
			defer connection.CloseNow()
			errorsChannel <- serve(request.Context(), connection)
		default:
			http.NotFound(response, request)
		}
	}))
}

func readCall(ctx context.Context, connection *websocket.Conn) (receivedCall, error) {
	var call receivedCall
	err := wsjson.Read(ctx, connection, &call)
	return call, err
}

func rawArguments(values ...any) []json.RawMessage {
	arguments := make([]json.RawMessage, 0, len(values))
	for _, value := range values {
		encoded, _ := json.Marshal(value)
		arguments = append(arguments, encoded)
	}
	return arguments
}

func writeDeckyVersion(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	services := filepath.Join(root, "services")
	if err := os.Mkdir(services, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(services, ".loader.version"), []byte(version+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeArchive(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "package.zip")
	contents := []byte("verified archive fixture")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(contents)
	return path, hex.EncodeToString(hash[:])
}

type protocolTestError struct{ message string }

func (failure *protocolTestError) Error() string { return failure.message }
