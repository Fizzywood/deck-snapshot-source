// Package deckyapi implements the version-bounded loopback protocol used to
// ask Decky Loader's root service to install verified plugin archives.
package deckyapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const (
	SupportedVersion   = "v3.2.6"
	productionBaseURL  = "http://127.0.0.1:1337"
	maxTokenBytes      = 64
	maxMessageBytes    = 64 << 10
	maxMessagesPerCall = 2048
	maxArchiveBytes    = int64(512 << 20)
	requestTimeout     = 10 * time.Minute
)

// Installer is the bounded mutation boundary consumed by restore planning and
// execution. Probe performs no plugin mutation and never opens a websocket.
type Installer interface {
	Probe(context.Context, string) error
	Install(context.Context, InstallRequest) error
	Uninstall(context.Context, string) error
}

// InstallRequest identifies one already-downloaded and checksum-verified ZIP.
// Replace selects Decky's overwrite confirmation type.
type InstallRequest struct {
	ArchivePath string
	Name        string
	Version     string
	SHA256      string
	Replace     bool
}

// Client talks only to Decky's fixed IPv4 loopback service.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
}

// New returns the production client with proxying, redirects, DNS, and remote
// destinations disabled.
func New() *Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   false,
		DisableKeepAlives:   true,
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "127.0.0.1:1337" {
				return nil, errors.New("Decky Loader connection escaped the fixed loopback address")
			}
			return dialer.DialContext(ctx, "tcp4", "127.0.0.1:1337")
		},
	}
	parsed, _ := url.Parse(productionBaseURL)
	return &Client{
		baseURL: parsed,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("Decky Loader loopback redirects are not allowed")
			},
		},
	}
}

func newTestClient(baseURL string, client *http.Client) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return nil, errors.New("test Decky Loader URL must be a plain IPv4 loopback origin")
	}
	if client == nil {
		return nil, errors.New("test Decky Loader HTTP client is required")
	}
	return &Client{baseURL: parsed, httpClient: client}, nil
}

// Probe validates the exact supported loader version and obtains a bounded
// loopback token without retaining it or displacing Decky's frontend socket.
func (client *Client) Probe(ctx context.Context, deckyHome string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateDeckyVersion(deckyHome); err != nil {
		return err
	}
	token, err := client.authToken(ctx)
	clear(token)
	return err
}

// Install asks Decky to confirm and install one private verified local archive.
func (client *Client) Install(ctx context.Context, request InstallRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	archive, err := openValidatedArchive(request)
	if err != nil {
		return err
	}
	defer archive.Close()
	connection, err := client.connect(ctx)
	if err != nil {
		return err
	}
	defer connection.CloseNow()

	installType := 0
	if request.Replace {
		installType = 4
	}
	artifact := (&url.URL{Scheme: "file", Path: filepath.ToSlash(heldArchivePath(archive, request.ArchivePath))}).String()
	promptID := ""
	finished := false
	if _, err := call(ctx, connection, 1, "utilities/install_plugin", []any{artifact, request.Name, request.Version, request.SHA256, installType}, func(message inboundMessage) error {
		if message.Event != "loader/add_plugin_install_prompt" || promptID != "" {
			return errors.New("Decky Loader returned an unexpected install event")
		}
		values, err := decodePrompt(message.Args)
		if err != nil || values.Name != request.Name || values.Version != request.Version || values.SHA256 != request.SHA256 || values.InstallType != installType {
			return errors.New("Decky Loader install prompt did not match the approved request")
		}
		promptID = values.RequestID
		return nil
	}); err != nil {
		return err
	}
	if promptID == "" {
		return errors.New("Decky Loader did not return an install confirmation identity")
	}
	if _, err := call(ctx, connection, 2, "utilities/confirm_plugin_install", []any{promptID}, func(message inboundMessage) error {
		if !validEventName(message.Event) {
			return errors.New("Decky Loader returned an unsafe progress event")
		}
		if message.Event == "loader/plugin_download_finish" {
			var name string
			if len(message.Args) != 1 || json.Unmarshal(message.Args[0], &name) != nil || name != request.Name {
				return errors.New("Decky Loader completion event did not match the approved plugin")
			}
			finished = true
		}
		return nil
	}); err != nil {
		return err
	}
	if !finished {
		return errors.New("Decky Loader did not confirm plugin installation completion")
	}
	return nil
}

// Uninstall removes one plugin through Decky's own root service. Callers must
// verify the resulting target identity because Decky v3.2.6 reports some
// internal uninstall failures only through its private logs.
func (client *Client) Uninstall(ctx context.Context, name string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !validField(name, 1, 128) {
		return errors.New("Decky Loader plugin name is unsafe")
	}
	connection, err := client.connect(ctx)
	if err != nil {
		return err
	}
	defer connection.CloseNow()
	if _, err := call(ctx, connection, 1, "utilities/uninstall_plugin", []any{name}, func(message inboundMessage) error {
		if !validEventName(message.Event) {
			return errors.New("Decky Loader returned an unsafe uninstall event")
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func validateDeckyVersion(deckyHome string) error {
	if !filepath.IsAbs(deckyHome) {
		return errors.New("Decky home must be absolute")
	}
	path := filepath.Join(deckyHome, "services", ".loader.version")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > 64 || !trustedVersionFile(info) {
		return errors.New("Decky Loader version file is missing or unsafe")
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("Decky Loader version file could not be opened")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("Decky Loader version file changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil || len(contents) > 64 || strings.TrimSpace(string(contents)) != SupportedVersion {
		return errors.New("Decky Loader version is not supported by this restore boundary")
	}
	return nil
}

func validateInstallRequest(request InstallRequest) error {
	file, err := openValidatedArchive(request)
	if file != nil {
		_ = file.Close()
	}
	return err
}

func openValidatedArchive(request InstallRequest) (*os.File, error) {
	if !validField(request.Name, 1, 128) || !validField(request.Version, 1, 128) {
		return nil, errors.New("Decky Loader plugin identity is unsafe")
	}
	decoded, err := hex.DecodeString(request.SHA256)
	if err != nil || len(decoded) != sha256.Size || request.SHA256 != strings.ToLower(request.SHA256) {
		return nil, errors.New("Decky Loader plugin checksum is invalid")
	}
	if !filepath.IsAbs(request.ArchivePath) {
		return nil, errors.New("Decky Loader plugin archive must be absolute")
	}
	info, err := os.Lstat(request.ArchivePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > maxArchiveBytes || !privateArchiveMode(info) {
		return nil, errors.New("Decky Loader plugin archive is missing or unsafe")
	}
	file, err := os.Open(request.ArchivePath)
	if err != nil {
		return nil, errors.New("Decky Loader plugin archive could not be opened")
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, errors.New("Decky Loader plugin archive changed while opening")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maxArchiveBytes+1))
	final, finalErr := file.Stat()
	if err != nil || finalErr != nil || written != info.Size() || written > maxArchiveBytes || !os.SameFile(info, final) || hex.EncodeToString(hash.Sum(nil)) != request.SHA256 {
		file.Close()
		return nil, errors.New("Decky Loader plugin archive failed final checksum validation")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, errors.New("Decky Loader plugin archive could not be rewound")
	}
	return file, nil
}

func validField(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func (client *Client) authToken(ctx context.Context) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	endpoint := *client.baseURL
	endpoint.Path = "/auth/token"
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("create Decky Loader token request")
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, errors.New("Decky Loader loopback service is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("Decky Loader token endpoint rejected the request")
	}
	token, err := io.ReadAll(io.LimitReader(response.Body, maxTokenBytes+1))
	if err != nil || !validToken(token) {
		clear(token)
		return nil, errors.New("Decky Loader returned an invalid authentication token")
	}
	return token, nil
}

func validToken(token []byte) bool {
	if len(token) != 36 {
		return false
	}
	for index, character := range token {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func (client *Client) connect(ctx context.Context) (*websocket.Conn, error) {
	token, err := client.authToken(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := *client.baseURL
	endpoint.Scheme = "ws"
	endpoint.Path = "/ws"
	query := endpoint.Query()
	query.Set("auth", string(token))
	endpoint.RawQuery = query.Encode()
	connection, response, err := websocket.Dial(ctx, endpoint.String(), &websocket.DialOptions{HTTPClient: client.httpClient, CompressionMode: websocket.CompressionDisabled})
	clear(token)
	endpoint.RawQuery = ""
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return nil, errors.New("Decky Loader websocket connection failed")
	}
	connection.SetReadLimit(maxMessageBytes)
	return connection, nil
}

type outboundCall struct {
	Type  int    `json:"type"`
	ID    int    `json:"id"`
	Route string `json:"route"`
	Args  []any  `json:"args"`
}

type inboundMessage struct {
	Type   int               `json:"type"`
	ID     int               `json:"id,omitempty"`
	Result json.RawMessage   `json:"result,omitempty"`
	Error  json.RawMessage   `json:"error,omitempty"`
	Event  string            `json:"event,omitempty"`
	Args   []json.RawMessage `json:"args,omitempty"`
}

func call(ctx context.Context, connection *websocket.Conn, id int, route string, arguments []any, onEvent func(inboundMessage) error) (json.RawMessage, error) {
	callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := wsjson.Write(callCtx, connection, outboundCall{Type: 0, ID: id, Route: route, Args: arguments}); err != nil {
		return nil, errors.New("write Decky Loader request")
	}
	for count := 0; count < maxMessagesPerCall; count++ {
		var message inboundMessage
		if err := wsjson.Read(callCtx, connection, &message); err != nil {
			return nil, errors.New("read Decky Loader response")
		}
		switch message.Type {
		case -1:
			if message.ID != id {
				return nil, errors.New("Decky Loader returned an unrelated error response")
			}
			return nil, errors.New("Decky Loader rejected the bounded request")
		case 1:
			if message.ID != id {
				return nil, errors.New("Decky Loader returned an unrelated reply")
			}
			return message.Result, nil
		case 3:
			if onEvent == nil || onEvent(message) != nil {
				return nil, errors.New("Decky Loader returned an unexpected event")
			}
		default:
			return nil, errors.New("Decky Loader returned an unknown message type")
		}
	}
	return nil, errors.New("Decky Loader response exceeded the message limit")
}

type promptValues struct {
	Name        string
	Version     string
	RequestID   string
	SHA256      string
	InstallType int
}

func decodePrompt(arguments []json.RawMessage) (promptValues, error) {
	var values promptValues
	if len(arguments) != 5 || json.Unmarshal(arguments[0], &values.Name) != nil || json.Unmarshal(arguments[1], &values.Version) != nil || json.Unmarshal(arguments[2], &values.RequestID) != nil || json.Unmarshal(arguments[3], &values.SHA256) != nil || json.Unmarshal(arguments[4], &values.InstallType) != nil || !validField(values.RequestID, 1, 64) {
		return promptValues{}, errors.New("invalid Decky Loader install prompt")
	}
	for _, character := range values.RequestID {
		if (character < '0' || character > '9') && character != '.' {
			return promptValues{}, errors.New("invalid Decky Loader install request identity")
		}
	}
	return values, nil
}

func validEventName(value string) bool {
	return len(value) >= len("loader/")+1 && len(value) <= 128 && strings.HasPrefix(value, "loader/") && validField(value, 1, 128)
}
