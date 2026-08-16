package cloud

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
)

const testGoogleDesktopCredential = "synthetic-desktop-credential"

type connectionRunner struct {
	configPath                        string
	created                           []string
	failDrive                         bool
	stdin                             string
	protectedOAuthOutsidePrivateInput bool
	sawDriveFileScope                 bool
	sawVisibleBasePath                bool
	sawSnapshotFolder                 bool
	healthChecks                      int
	failHealth                        bool
	legacy                            bool
}

func (runner *connectionRunner) Run(_ context.Context, request Request) (Result, error) {
	if len(request.Args) == 0 {
		return Result{}, errors.New("missing command")
	}
	protectedValues := []string{testGoogleDesktopCredential, "synthetic-access", "synthetic-refresh"}
	for _, argument := range request.Args {
		if strings.Contains(argument, "client_secret") {
			runner.protectedOAuthOutsidePrivateInput = true
		}
		for _, protected := range protectedValues {
			if strings.Contains(argument, protected) {
				runner.protectedOAuthOutsidePrivateInput = true
			}
		}
	}
	for _, protected := range protectedValues {
		if strings.Contains(string(request.Stdin), protected) {
			runner.protectedOAuthOutsidePrivateInput = true
		}
	}
	for _, value := range request.SecretEnv {
		for _, protected := range protectedValues {
			if strings.Contains(value, protected) {
				runner.protectedOAuthOutsidePrivateInput = true
			}
		}
	}
	if request.Args[0] == "version" {
		return Result{Stdout: expectedRcloneVersion + "\n"}, nil
	}
	if request.Args[0] == "obscure" {
		return Result{Stdout: "obscured-recovery-value\n"}, nil
	}
	if request.Args[0] == "secure-config-create" {
		var input secureConfigInput
		if err := json.Unmarshal(request.SecretJSON, &input); err != nil {
			return Result{}, err
		}
		if input.Type != "drive" || input.Parameters["scope"] != "drive.file" || input.Parameters["client_secret"] != testGoogleDesktopCredential || input.Parameters["token"] == "" {
			return Result{}, errors.New("invalid secure configuration input")
		}
		runner.sawDriveFileScope = true
		if runner.failDrive {
			return Result{}, errors.New("synthetic protected configuration failure")
		}
		runner.created = append(runner.created, input.Name)
		return Result{}, nil
	}
	if request.Args[0] == "config" && len(request.Args) >= 3 && request.Args[1] == "encryption" && request.Args[2] == "set" {
		runner.stdin = string(request.Stdin)
		if err := os.WriteFile(runner.configPath, []byte("synthetic encrypted config"), 0o600); err != nil {
			return Result{}, err
		}
		return Result{}, nil
	}
	if request.Args[0] == "config" && len(request.Args) >= 4 && request.Args[1] == "create" {
		if request.Args[3] == "drive" {
			for index, argument := range request.Args {
				if argument == "client_secret" {
					runner.protectedOAuthOutsidePrivateInput = true
				}
				if argument == "scope" && index+1 < len(request.Args) && request.Args[index+1] == "drive.file" {
					runner.sawDriveFileScope = true
				}
			}
		}
		if request.Args[3] == "crypt" {
			for index, argument := range request.Args {
				if argument == "remote" && index+1 < len(request.Args) && request.Args[index+1] == "deck-snapshot-drive:"+GoogleDriveBasePath {
					runner.sawVisibleBasePath = true
				}
			}
		}
		if request.Args[3] == "drive" && runner.failDrive {
			return Result{}, errors.New("synthetic OAuth failure")
		}
		runner.created = append(runner.created, request.Args[2])
		return Result{}, nil
	}
	if request.Args[0] == "config" && len(request.Args) == 3 && request.Args[1] == "redacted" {
		switch request.Args[2] {
		case "deck-snapshot-drive":
			if runner.legacy {
				return Result{Stdout: "[deck-snapshot-drive]\ntype = drive\nscope = drive.appfolder\nclient_id = synthetic.apps.googleusercontent.com\n"}, nil
			}
			return Result{Stdout: "[deck-snapshot-drive]\ntype = drive\nscope = drive.file\nclient_id = synthetic.apps.googleusercontent.com\n"}, nil
		case "deck-snapshot-crypt":
			if runner.legacy {
				return Result{Stdout: "[deck-snapshot-crypt]\ntype = crypt\nremote = deck-snapshot-drive:\nfilename_encryption = standard\ndirectory_name_encryption = true\n"}, nil
			}
			return Result{Stdout: "[deck-snapshot-crypt]\ntype = crypt\nremote = deck-snapshot-drive:Deck Snapshot/Snapshots\nfilename_encryption = standard\ndirectory_name_encryption = true\n"}, nil
		}
	}
	if request.Args[0] == "listremotes" {
		return Result{Stdout: "deck-snapshot-drive:\ndeck-snapshot-crypt:\n"}, nil
	}
	if request.Args[0] == "mkdir" && len(request.Args) == 2 && request.Args[1] == "deck-snapshot-crypt:" {
		runner.sawSnapshotFolder = true
		return Result{}, nil
	}
	if request.Args[0] == "lsjson" {
		runner.healthChecks++
		if runner.failHealth {
			return Result{}, errors.New("synthetic revoked authorization")
		}
		return Result{Stdout: "[]"}, nil
	}
	if request.Args[0] == "config" && len(request.Args) == 3 && request.Args[1] == "disconnect" {
		return Result{}, nil
	}
	return Result{}, errors.New("unexpected command")
}

func TestConnectGoogleAndDisconnect(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "cloud", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &connectionRunner{configPath: configPath}
	manager := Manager{
		Runner: runner, SnapshotDirectory: filepath.Join(root, "data", "snapshots"), StateDirectory: filepath.Join(root, "state"), ConfigPath: configPath,
		RecoveryPath:   filepath.Join(root, "config", "cloud", ManagedRecoveryFileName),
		ConfigPassword: "synthetic-config-password", CryptRemote: "deck-snapshot-crypt", BaseRemote: "deck-snapshot-drive", BasePath: GoogleDriveBasePath, ExpectedBaseType: "drive",
		ProtectionFingerprint: strings.Repeat("a", 64), CryptPassword: "obscured-primary", CryptPassword2: "obscured-secondary",
		AllowUnencryptedTest: true, Limits: limits.Default(),
	}
	manager.googleOAuth = testGoogleOAuthDependencies(t)
	status, err := manager.ConnectGoogleWithInitialization(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential, time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
	if err != nil || !status.Configured || !status.Protected || !status.RecoveryAcknowledged || status.Legacy || status.Scope != "drive.file" || status.Folder != "My Drive/"+GoogleDriveBasePath {
		t.Fatalf("ConnectGoogle() = %#v, %v", status, err)
	}
	if runner.stdin != manager.ConfigPassword+"\n"+manager.ConfigPassword+"\n" || len(runner.created) != 2 {
		t.Fatalf("connect did not use the bounded encrypted configuration flow: stdin=%q created=%v", runner.stdin, runner.created)
	}
	if runner.protectedOAuthOutsidePrivateInput || !runner.sawDriveFileScope || !runner.sawVisibleBasePath {
		t.Fatal("Google authorization did not keep the Desktop credential off process arguments or bind drive.file to the fixed visible folder")
	}
	if !runner.sawSnapshotFolder || runner.healthChecks != 1 {
		t.Fatal("Google connection did not create and read back the protected fixed snapshot folder")
	}
	if err := manager.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration remained after disconnect: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "state", "cloud", "recovery-acknowledgement.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("acknowledgement remained after disconnect: %v", err)
	}
}

func TestConnectGoogleRequiresExplicitSetupForAnEmptyAccount(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "cloud", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &connectionRunner{configPath: configPath}
	manager := Manager{
		Runner: runner, SnapshotDirectory: filepath.Join(root, "snapshots"), StateDirectory: filepath.Join(root, "state"), ConfigPath: configPath,
		RecoveryPath:   filepath.Join(root, "config", "cloud", ManagedRecoveryFileName),
		ConfigPassword: "synthetic-config-password", CryptRemote: "deck-snapshot-crypt", BaseRemote: "deck-snapshot-drive", BasePath: GoogleDriveBasePath, ExpectedBaseType: "drive",
		AllowUnencryptedTest: true, Limits: limits.Default(),
	}
	manager.googleOAuth = testGoogleOAuthDependenciesWithState(t, false, nil)
	if _, err := manager.ConnectGoogle(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential, time.Now()); err == nil || !strings.Contains(err.Error(), "confirm setup") {
		t.Fatalf("empty account without confirmation = %v", err)
	}
	if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty account left a cloud configuration: %v", err)
	}
}

func TestConnectGoogleRecoversFromExistingAppDataWithoutManualFile(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "cloud", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	material, err := GenerateRecovery(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runner := &connectionRunner{configPath: configPath}
	manager := Manager{
		Runner: runner, SnapshotDirectory: filepath.Join(root, "snapshots"), StateDirectory: filepath.Join(root, "state"), ConfigPath: configPath,
		RecoveryPath:   filepath.Join(root, "config", "cloud", ManagedRecoveryFileName),
		ConfigPassword: "synthetic-config-password", CryptRemote: "deck-snapshot-crypt", BaseRemote: "deck-snapshot-drive", BasePath: GoogleDriveBasePath, ExpectedBaseType: "drive",
		AllowUnencryptedTest: true, Limits: limits.Default(),
	}
	manager.googleOAuth = testGoogleOAuthDependenciesWithState(t, false, &material)
	status, err := manager.ConnectGoogle(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential, time.Now())
	if err != nil || !status.RecoveryAcknowledged {
		t.Fatalf("same-account appData recovery = %#v, %v", status, err)
	}
	loaded, err := LoadRecovery(manager.RecoveryPath)
	if err != nil || loaded != material {
		t.Fatalf("managed recovery copy = %#v, %v", loaded, err)
	}
}

func TestConnectGoogleNeverGeneratesReplacementForVisibleSnapshots(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "cloud", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &connectionRunner{configPath: configPath}
	manager := Manager{
		Runner: runner, SnapshotDirectory: filepath.Join(root, "snapshots"), StateDirectory: filepath.Join(root, "state"), ConfigPath: configPath,
		RecoveryPath:   filepath.Join(root, "config", "cloud", ManagedRecoveryFileName),
		ConfigPassword: "synthetic-config-password", CryptRemote: "deck-snapshot-crypt", BaseRemote: "deck-snapshot-drive", BasePath: GoogleDriveBasePath, ExpectedBaseType: "drive",
		AllowUnencryptedTest: true, Limits: limits.Default(),
	}
	manager.googleOAuth = testGoogleOAuthDependenciesWithState(t, true, nil)
	if _, err := manager.ConnectGoogleWithInitialization(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential, time.Now()); err == nil || !strings.Contains(err.Error(), "original recovery key") {
		t.Fatalf("visible snapshots without recovery key = %v", err)
	}
	if _, err := os.Lstat(manager.RecoveryPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement recovery key was generated: %v", err)
	}
}

func TestConnectGoogleRejectsUnreachableAuthorizationBeforeAcknowledgement(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "cloud", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &connectionRunner{configPath: configPath, failHealth: true}
	manager := Manager{
		Runner: runner, SnapshotDirectory: filepath.Join(root, "data", "snapshots"), StateDirectory: filepath.Join(root, "state"), ConfigPath: configPath,
		ConfigPassword: "synthetic-config-password", CryptRemote: "deck-snapshot-crypt", BaseRemote: "deck-snapshot-drive", BasePath: GoogleDriveBasePath, ExpectedBaseType: "drive",
		ProtectionFingerprint: strings.Repeat("a", 64), CryptPassword: "obscured-primary", CryptPassword2: "obscured-secondary",
		AllowUnencryptedTest: true, Limits: limits.Default(),
	}
	manager.googleOAuth = testGoogleOAuthDependencies(t)
	if _, err := manager.ConnectGoogleWithInitialization(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential, time.Now()); err == nil || !strings.Contains(err.Error(), "reachability") {
		t.Fatalf("ConnectGoogle() error = %v", err)
	}
	if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unusable configuration remained after failed health check: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "state", "cloud", "recovery-acknowledgement.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed health check created a recovery acknowledgement: %v", err)
	}
}

func TestLegacyAppFolderRemainsReadableButRejectsNewUploads(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "cloud", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("synthetic encrypted config"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := Manager{
		Runner: &connectionRunner{configPath: configPath, legacy: true}, SnapshotDirectory: filepath.Join(root, "snapshots"), StateDirectory: filepath.Join(root, "state"), ConfigPath: configPath,
		ConfigPassword: "synthetic-config-password", CryptRemote: "deck-snapshot-crypt", BaseRemote: "deck-snapshot-drive", BasePath: GoogleDriveBasePath, ExpectedBaseType: "drive",
		ProtectionFingerprint: strings.Repeat("a", 64), CryptPassword: "obscured-primary", CryptPassword2: "obscured-secondary",
		AllowUnencryptedTest: true, Limits: limits.Default(),
	}
	status, err := manager.Check(context.Background())
	if err != nil || !status.Legacy || status.Scope != "drive.appfolder" || status.Folder != "hidden application data" {
		t.Fatalf("legacy Check() = %#v, %v", status, err)
	}
	if _, err := manager.Upload(context.Background(), filepath.Join(root, "snapshot.tar.gz")); err == nil || !strings.Contains(err.Error(), "legacy hidden app-folder") {
		t.Fatalf("legacy Upload() error = %v", err)
	}
}

func TestConnectGoogleCleansPartialConfiguration(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &connectionRunner{configPath: configPath, failDrive: true}
	manager := Manager{
		Runner: runner, SnapshotDirectory: filepath.Join(root, "snapshots"), StateDirectory: filepath.Join(root, "state"), ConfigPath: configPath,
		ConfigPassword: "synthetic-config-password", CryptRemote: "deck-snapshot-crypt", BaseRemote: "deck-snapshot-drive", BasePath: GoogleDriveBasePath, ExpectedBaseType: "drive",
		ProtectionFingerprint: strings.Repeat("a", 64), CryptPassword: "obscured-primary", CryptPassword2: "obscured-secondary",
		AllowUnencryptedTest: true, Limits: limits.Default(),
	}
	manager.googleOAuth = testGoogleOAuthDependencies(t)
	if _, err := manager.ConnectGoogleWithInitialization(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential, time.Now()); err == nil || !strings.Contains(err.Error(), "encrypted cloud configuration") {
		t.Fatalf("ConnectGoogle() error = %v", err)
	}
	if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial configuration remained after failed connect: %v", err)
	}
}

func testGoogleOAuthDependencies(t *testing.T) googleOAuthDependencies {
	return testGoogleOAuthDependenciesWithState(t, false, nil)
}

func testGoogleOAuthDependenciesWithState(t *testing.T, existingSnapshots bool, existingMaterial *RecoveryMaterial) googleOAuthDependencies {
	t.Helper()
	expectedChallenge := ""
	var recoveryBody []byte
	if existingMaterial != nil {
		var encodeErr error
		recoveryBody, encodeErr = encodeRecovery(*existingMaterial)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
	}
	tokenServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/token" {
			if request.Method == http.MethodPost {
				if request.URL.Path != "/upload/drive/v3/files" || request.URL.Query().Get("uploadType") != "multipart" {
					response.WriteHeader(http.StatusBadRequest)
					return
				}
				mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
				if err != nil || mediaType != "multipart/related" {
					response.WriteHeader(http.StatusBadRequest)
					return
				}
				reader := multipart.NewReader(request.Body, parameters["boundary"])
				for {
					part, partErr := reader.NextPart()
					if errors.Is(partErr, io.EOF) {
						break
					}
					if partErr != nil {
						response.WriteHeader(http.StatusBadRequest)
						return
					}
					contents, readErr := io.ReadAll(part)
					_ = part.Close()
					if readErr == nil && bytes.Contains(contents, []byte(`"crypt_password"`)) {
						recoveryBody = contents
					}
				}
				response.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(response, `{"id":"synthetic-recovery","name":"deck-snapshot-recovery-v1.json","mimeType":"application/json"}`)
				return
			}
			if request.Method == http.MethodGet && request.URL.Query().Get("alt") == "media" {
				if len(recoveryBody) == 0 {
					response.WriteHeader(http.StatusNotFound)
					return
				}
				response.Header().Set("Content-Type", "application/json")
				_, _ = response.Write(recoveryBody)
				return
			}
			if request.Method == http.MethodGet && request.URL.Query().Get("spaces") == "appDataFolder" && len(recoveryBody) > 0 {
				response.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(response, `{"files":[{"id":"synthetic-recovery","name":"deck-snapshot-recovery-v1.json","mimeType":"application/json","size":"%d"}]}`, len(recoveryBody))
				return
			}
			if request.Method == http.MethodGet && request.URL.Query().Get("spaces") == "drive" && existingSnapshots {
				query := request.URL.Query().Get("q")
				switch {
				case strings.Contains(query, "'root' in parents"):
					_, _ = io.WriteString(response, `{"files":[{"id":"synthetic-root","name":"Deck Snapshot","mimeType":"application/vnd.google-apps.folder"}]}`)
				case strings.Contains(query, "'synthetic-root' in parents"):
					_, _ = io.WriteString(response, `{"files":[{"id":"synthetic-snapshots","name":"Snapshots","mimeType":"application/vnd.google-apps.folder"}]}`)
				case strings.Contains(query, "'synthetic-snapshots' in parents"):
					_, _ = io.WriteString(response, `{"files":[{"id":"synthetic-object","name":"encrypted-object","mimeType":"application/octet-stream","size":"1"}]}`)
				default:
					_, _ = io.WriteString(response, `{"files":[]}`)
				}
				return
			}
			response.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(response, `{"files":[]}`)
			return
		}
		if request.Method != http.MethodPost {
			t.Errorf("token exchange method = %s", request.Method)
			response.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := request.ParseForm(); err != nil {
			t.Error(err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		verifier := request.Form.Get("code_verifier")
		challengeBytes := sha256.Sum256([]byte(verifier))
		challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
		if request.Form.Get("client_id") != "synthetic.apps.googleusercontent.com" || request.Form.Get("code") != "synthetic-code" ||
			request.Form.Get("client_secret") != testGoogleDesktopCredential || verifier == "" || challenge != expectedChallenge {
			t.Errorf("unsafe PKCE token exchange fields: %v", request.Form)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","scope":"https://www.googleapis.com/auth/drive.file https://www.googleapis.com/auth/drive.appdata","expires_in":3600}`)
	}))
	t.Cleanup(tokenServer.Close)
	return googleOAuthDependencies{
		authorizationEndpoint: googleAuthorizationEndpoint,
		tokenEndpoint:         tokenServer.URL + "/token",
		driveAPIEndpoint:      tokenServer.URL + "/drive/v3/files",
		driveUploadEndpoint:   tokenServer.URL + "/upload/drive/v3/files",
		client:                tokenServer.Client(),
		now:                   func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) },
		timeout:               5 * time.Second,
		openURL: func(value string) error {
			parsed, err := url.Parse(value)
			if err != nil {
				return err
			}
			query := parsed.Query()
			expectedChallenge = query.Get("code_challenge")
			if parsed.Scheme != "https" || parsed.Hostname() != "accounts.google.com" || query.Get("scope") != strings.Join(googleDriveOAuthScopes, " ") ||
				query.Get("code_challenge_method") != "S256" || expectedChallenge == "" || query.Get("client_secret") != "" {
				return errors.New("unsafe authorization URL")
			}
			callback, err := url.Parse(query.Get("redirect_uri"))
			if err != nil {
				return err
			}
			callbackQuery := callback.Query()
			callbackQuery.Set("state", query.Get("state"))
			callbackQuery.Set("code", "synthetic-code")
			callback.RawQuery = callbackQuery.Encode()
			callbackResponse, err := http.Get(callback.String())
			if err != nil {
				return err
			}
			_, _ = io.Copy(io.Discard, callbackResponse.Body)
			_ = callbackResponse.Body.Close()
			if callbackResponse.StatusCode != http.StatusOK {
				return errors.New("callback failed")
			}
			return nil
		},
	}
}
