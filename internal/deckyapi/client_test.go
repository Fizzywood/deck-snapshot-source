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
	Type    int               `json:"type"`
	ID      int               `json:"id"`
	Route   string            `json:"route"`
	Args    []json.RawMessage `json:"args"`
	RawArgs json.RawMessage   `json:"-"`
}

type receivedCallWire struct {
	Type  int             `json:"type"`
	ID    int             `json:"id"`
	Route string          `json:"route"`
	Args  json.RawMessage `json:"args"`
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
	serverErrors := make(chan error, 2)
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

func TestInventoryUsesOnlyFixedRoute(t *testing.T) {
	t.Parallel()
	serverErrors := make(chan error, 1)
	server := newProtocolServer(t, func(ctx context.Context, connection *websocket.Conn) error {
		call, err := readCall(ctx, connection)
		if err != nil {
			return err
		}
		if call.Type != 0 || call.ID != 1 || call.Route != "loader/get_plugins" || string(call.RawArgs) != "[]" || len(call.Args) != 0 {
			return &protocolTestError{"unexpected inventory request"}
		}
		return wsjson.Write(ctx, connection, inboundMessage{Type: 1, ID: 1, Result: json.RawMessage(`[{"name":"CSS Loader","version":"1.0","disabled":true}]`)})
	}, serverErrors)
	t.Cleanup(server.Close)
	client, err := newTestClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	plugins, err := client.Inventory(context.Background())
	if err != nil || len(plugins) != 1 || plugins[0].Name != "CSS Loader" || !plugins[0].Disabled {
		t.Fatalf("Inventory() = %#v, %v", plugins, err)
	}
	if err := <-serverErrors; err != nil {
		t.Fatal(err)
	}
}

func TestInventoryFailsClosedOnDeckyErrorReply(t *testing.T) {
	t.Parallel()
	serverErrors := make(chan error, 1)
	server := newProtocolServer(t, func(ctx context.Context, connection *websocket.Conn) error {
		call, err := readCall(ctx, connection)
		if err != nil {
			return err
		}
		if call.Route != "loader/get_plugins" || string(call.RawArgs) != "[]" {
			return &protocolTestError{"unexpected inventory error-request"}
		}
		return wsjson.Write(ctx, connection, inboundMessage{Type: -1, ID: 1, Error: json.RawMessage(`{"name":"TypeError","message":"argument expansion failed"}`)})
	}, serverErrors)
	t.Cleanup(server.Close)
	client, err := newTestClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Inventory(context.Background()); err == nil || !strings.Contains(err.Error(), "TypeError: argument expansion failed") {
		t.Fatalf("Inventory() error = %v", err)
	}
	if err := <-serverErrors; err != nil {
		t.Fatal(err)
	}
}

func TestDisabledPluginNamesAreStrictAndCanonical(t *testing.T) {
	t.Parallel()
	values, err := normalizePluginNames([]string{"CSS Loader", "Alarm Me"})
	if err != nil || !samePluginNames(values, []string{"Alarm Me", "CSS Loader"}) {
		t.Fatalf("normalizePluginNames() = %#v, %v", values, err)
	}
	if _, err := normalizePluginNames([]string{"CSS Loader", "CSS Loader"}); err == nil {
		t.Fatal("duplicate disabled plugin state was accepted")
	}
	if _, err := normalizePluginNames([]string{" CSS Loader"}); err == nil {
		t.Fatal("unsafe disabled plugin state was accepted")
	}
}

func TestRestartUsesBoundedRouteAndObservesReplacement(t *testing.T) {
	t.Parallel()
	deckyHome := writeDeckyVersion(t, SupportedVersion)
	var restarting atomic.Bool
	var offlineProbes atomic.Int32
	serverErrors := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/auth/token":
			if restarting.Load() && offlineProbes.Add(1) == 1 {
				http.Error(response, "restarting", http.StatusServiceUnavailable)
				return
			}
			_, _ = response.Write([]byte(testToken))
		case "/ws":
			if request.URL.Query().Get("auth") != testToken {
				http.Error(response, "unauthorized", http.StatusUnauthorized)
				return
			}
			connection, err := websocket.Accept(response, request, nil)
			if err != nil {
				serverErrors <- err
				return
			}
			defer connection.CloseNow()
			call, err := readCall(request.Context(), connection)
			if err != nil {
				serverErrors <- err
				return
			}
			switch call.Route {
			case "updater/do_restart":
				if call.Type != 0 || call.ID != 1 || string(call.RawArgs) != "[]" || len(call.Args) != 0 {
					serverErrors <- &protocolTestError{"unexpected restart request"}
					return
				}
				restarting.Store(true)
				serverErrors <- wsjson.Write(request.Context(), connection, inboundMessage{Type: 1, ID: 1, Result: json.RawMessage("null")})
			case "utilities/restart_webhelper":
				if call.Type != 0 || call.ID != 1 || string(call.RawArgs) != "[]" || len(call.Args) != 0 {
					serverErrors <- &protocolTestError{"unexpected Steam reload request"}
					return
				}
				serverErrors <- wsjson.Write(request.Context(), connection, inboundMessage{Type: 1, ID: 1, Result: json.RawMessage("null")})
			default:
				serverErrors <- &protocolTestError{"unexpected runtime request"}
			}
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(server.Close)
	client, err := newTestClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Restart(context.Background(), deckyHome); err != nil {
		t.Fatalf("Restart() error = %v", err)
	}
	if offlineProbes.Load() < 2 {
		t.Fatalf("restart did not observe an unavailable then live replacement: %d probes", offlineProbes.Load())
	}
	for range 2 {
		if err := <-serverErrors; err != nil {
			t.Fatal(err)
		}
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
	var wire receivedCallWire
	if err := wsjson.Read(ctx, connection, &wire); err != nil {
		return receivedCall{}, err
	}
	if string(wire.Args) == "null" || len(wire.Args) == 0 {
		return receivedCall{}, &protocolTestError{"call arguments were not an array"}
	}
	var arguments []json.RawMessage
	if err := json.Unmarshal(wire.Args, &arguments); err != nil {
		return receivedCall{}, err
	}
	return receivedCall{Type: wire.Type, ID: wire.ID, Route: wire.Route, Args: arguments, RawArgs: wire.Args}, nil
}

func TestNilCallArgumentsSerializeAsEmptyArray(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(outboundCall{Type: 0, ID: 1, Route: "loader/get_plugins", Args: normalizeCallArguments(nil)})
	if err != nil || !strings.Contains(string(encoded), `"args":[]`) || strings.Contains(string(encoded), `"args":null`) {
		t.Fatalf("encoded nil-argument call = %s, error=%v", encoded, err)
	}
}

func TestParameterizedCallArgumentsRemainUnchanged(t *testing.T) {
	t.Parallel()
	arguments := []any{"disabled_plugins", []string{"CSS Loader"}}
	encoded, err := json.Marshal(outboundCall{Type: 0, ID: 1, Route: "utilities/settings/set", Args: normalizeCallArguments(arguments)})
	if err != nil || !strings.Contains(string(encoded), `"args":["disabled_plugins",["CSS Loader"]]`) {
		t.Fatalf("encoded parameterized call = %s, error=%v", encoded, err)
	}
}

func TestDeckyErrorDetailsAreBoundedAndFailClosed(t *testing.T) {
	t.Parallel()
	if detail := boundedDeckyError(json.RawMessage(`{"name":"TypeError","message":"argument expansion failed"}`)); detail != "TypeError: argument expansion failed" {
		t.Fatalf("boundedDeckyError() = %q", detail)
	}
	if detail := boundedDeckyError(json.RawMessage(`{"name":"TypeError","message":"token=must-not-appear"}`)); detail != "" {
		t.Fatalf("sensitive Decky error detail was accepted: %q", detail)
	}
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
