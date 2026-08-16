package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCommandRunnerRedactsBoundsAndTimesOut(t *testing.T) {
	runner := &CommandRunner{
		Binary: os.Args[0],
		AllowedCommand: map[string]struct{}{
			"-test.run=TestCommandRunnerHelperProcess": {},
		},
		MaxOutput: 128, DefaultTimeout: 10 * time.Second,
	}
	t.Run("redaction and secret environment", func(t *testing.T) {
		result, err := runner.Run(context.Background(), Request{
			Args:      []string{"-test.run=TestCommandRunnerHelperProcess", "--", "redact"},
			SecretEnv: map[string]string{"RCLONE_CONFIG_PASS": "private-test-password"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(result.Stdout+result.Stderr, "private-test-password") || strings.Contains(result.Stdout+result.Stderr, "token-value") || !strings.Contains(result.Stdout, "[REDACTED]") {
			t.Fatalf("command output was not safely redacted: %#v", result)
		}
	})
	t.Run("output bound", func(t *testing.T) {
		_, err := runner.Run(context.Background(), Request{Args: []string{"-test.run=TestCommandRunnerHelperProcess", "--", "overflow"}})
		if err == nil || !strings.Contains(err.Error(), "output exceeded") {
			t.Fatalf("expected bounded output failure, got %v", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		_, err := runner.Run(context.Background(), Request{Args: []string{"-test.run=TestCommandRunnerHelperProcess", "--", "sleep"}, Timeout: 20 * time.Millisecond})
		if err == nil || !strings.Contains(err.Error(), "deadline") {
			t.Fatalf("expected deadline failure, got %v", err)
		}
	})
	t.Run("OAuth URL failure redaction", func(t *testing.T) {
		result, err := runner.Run(context.Background(), Request{Args: []string{"-test.run=TestCommandRunnerHelperProcess", "--", "oauth-error"}})
		if err == nil || strings.Contains(err.Error()+result.Stderr, "transient-state") || strings.Contains(err.Error()+result.Stderr, "accounts.google.com") || !strings.Contains(err.Error(), "[REDACTED_OAUTH_URL]") {
			t.Fatalf("OAuth failure was not safely redacted: result=%#v error=%v", result, err)
		}
	})
	t.Run("desktop browser environment", func(t *testing.T) {
		t.Setenv("DISPLAY", ":0")
		t.Setenv("WAYLAND_DISPLAY", "wayland-0")
		t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
		t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
		t.Setenv("XAUTHORITY", "/run/user/1000/xauthority")
		t.Setenv("XDG_CURRENT_DESKTOP", "KDE")
		t.Setenv("DESKTOP_SESSION", "plasma")
		t.Setenv("KDE_FULL_SESSION", "true")
		t.Setenv("KDE_SESSION_VERSION", "6")
		t.Setenv("XDG_SESSION_TYPE", "wayland")
		t.Setenv("XDG_SESSION_CLASS", "user")
		t.Setenv("XDG_SESSION_DESKTOP", "KDE")
		t.Setenv("XDG_SESSION_ID", "5")
		t.Setenv("XDG_SEAT", "seat0")
		t.Setenv("XDG_SEAT_PATH", "/org/freedesktop/DisplayManager/Seat0")
		t.Setenv("XDG_SESSION_PATH", "/org/freedesktop/DisplayManager/Session1")
		t.Setenv("XDG_VTNR", "1")
		t.Setenv("XDG_MENU_PREFIX", "plasma-")
		t.Setenv("XDG_DATA_HOME", "/home/deck/.local/share")
		t.Setenv("XDG_DATA_DIRS", "/var/lib/flatpak/exports/share:/usr/local/share:/usr/share")
		t.Setenv("XDG_CONFIG_HOME", "/home/deck/.config")
		t.Setenv("XDG_CONFIG_DIRS", "/home/deck/.config/kdedefaults:/etc/xdg")
		t.Setenv("DECK_SNAPSHOT_UNSAFE_TEST_ENV", "must-not-pass")
		result, err := runner.Run(context.Background(), Request{
			Args: []string{"-test.run=TestCommandRunnerHelperProcess", "--", "desktop-env"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(result.Stdout) != "desktop environment present" {
			t.Fatalf("desktop environment helper output=%q", result.Stdout)
		}
	})
	if _, err := runner.Run(context.Background(), Request{Args: []string{"unsafe"}}); err == nil {
		t.Fatal("runner accepted a non-allowlisted command")
	}
	if _, err := runner.Run(context.Background(), Request{Args: []string{"-test.run=TestCommandRunnerHelperProcess"}, SecretEnv: map[string]string{"TOKEN": "unsafe"}}); err == nil {
		t.Fatal("runner accepted a non-allowlisted secret environment key")
	}
}

func TestRcloneRunnerAllowsOnlyRequiredCloudCommands(t *testing.T) {
	runner, err := NewRcloneRunner(filepath.Join(t.TempDir(), "rclone"), filepath.Join(t.TempDir(), "rclone.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"version", "config", "listremotes", "lsjson", "copyto", "mkdir", "about", "obscure", "secure-config-create"} {
		if _, allowed := runner.AllowedCommand[command]; !allowed {
			t.Fatalf("required rclone command %q is not allowlisted", command)
		}
	}
	if _, allowed := runner.AllowedCommand["delete"]; allowed {
		t.Fatal("destructive rclone delete command is allowlisted")
	}
}

func TestCommandRunnerSecureConfigCreatePersistsEncryptedToken(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("private Unix-socket configuration is Linux-only")
	}
	binary := os.Getenv("DECK_SNAPSHOT_RCLONE")
	if binary == "" {
		t.Skip("pinned rclone is not available")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	privateTempRoot, err := os.MkdirTemp("/tmp", "dst-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(privateTempRoot) })
	t.Setenv("TMPDIR", privateTempRoot)
	configPath := filepath.Join(root, "cloud", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRcloneRunner(binary, configPath)
	if err != nil {
		t.Fatal(err)
	}
	password := "synthetic-configuration-password"
	if _, err := runner.Run(context.Background(), Request{
		Args: []string{"config", "encryption", "set"}, Stdin: []byte(password + "\n" + password + "\n"),
	}); err != nil {
		t.Fatal(err)
	}
	token, err := json.Marshal(rcloneOAuthToken{
		AccessToken: "synthetic-access", TokenType: "Bearer", RefreshToken: "synthetic-refresh", Expiry: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(secureConfigInput{
		Name: "test-drive", Type: "drive",
		Parameters: map[string]string{"client_id": "synthetic.apps.googleusercontent.com", "client_secret": testGoogleDesktopCredential, "scope": "drive.file", "config_is_local": "true", "token": string(token)},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), Request{
		Args: []string{"secure-config-create"}, SecretJSON: encoded,
		SecretEnv: map[string]string{"RCLONE_CONFIG_PASS": password}, Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Stdout+result.Stderr, "synthetic-access") || strings.Contains(result.Stdout+result.Stderr, "synthetic-refresh") || strings.Contains(result.Stdout+result.Stderr, testGoogleDesktopCredential) {
		t.Fatal("secure configuration result exposed protected OAuth material")
	}
	redacted, err := runner.Run(context.Background(), Request{
		Args: []string{"config", "redacted", "test-drive"}, SecretEnv: map[string]string{"RCLONE_CONFIG_PASS": password},
	})
	if err != nil || !strings.Contains(redacted.Stdout, "token = XXX") || !strings.Contains(redacted.Stdout, "client_secret = XXX") || strings.Contains(redacted.Stdout, testGoogleDesktopCredential) {
		t.Fatalf("encrypted OAuth material was not persisted safely: %q, %v", redacted.Stdout, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(privateTempRoot, "ds-rc-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("private configuration channel was not cleaned up: %v, %v", leftovers, err)
	}
}

func TestValidateSecureConfigInputRejectsNonCanonicalOrUnsafeInput(t *testing.T) {
	expiry := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	validToken := `{"access_token":"synthetic-access","token_type":"Bearer","refresh_token":"synthetic-refresh","expiry":` + strconv.Quote(expiry) + `}`
	valid := `{"name":"test-drive","type":"drive","parameters":{"client_id":"synthetic.apps.googleusercontent.com","client_secret":"` + testGoogleDesktopCredential + `","scope":"drive.file","config_is_local":"true","token":` + strconv.Quote(validToken) + `}}`
	for name, encoded := range map[string]string{
		"trailing document":   valid + `{}`,
		"unknown input field": strings.TrimSuffix(valid, "}") + `,"unexpected":true}`,
		"unknown token field": strings.Replace(valid, `\"expiry\":\"`+expiry+`\"`, `\"expiry\":\"`+expiry+`\",\"unexpected\":true`, 1),
		"broader scope":       strings.Replace(valid, `"scope":"drive.file"`, `"scope":"drive"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateSecureConfigInput([]byte(encoded)); err == nil {
				t.Fatal("unsafe secure configuration input was accepted")
			}
		})
	}
}

func TestCommandRunnerHelperProcess(t *testing.T) {
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	switch os.Args[separator+1] {
	case "redact":
		if os.Getenv("RCLONE_CONFIG_PASS") != "private-test-password" {
			os.Exit(4)
		}
		fmt.Print("client_secret=token-value")
		fmt.Fprint(os.Stderr, " access_token=token-value")
	case "overflow":
		fmt.Print(strings.Repeat("x", 4096))
	case "sleep":
		time.Sleep(time.Second)
	case "oauth-error":
		fmt.Fprint(os.Stderr, "open https://accounts.google.com/o/oauth2/v2/auth?state=transient-state")
		os.Exit(2)
	case "desktop-env":
		trustedPath := strings.Join([]string{filepath.Dir(os.Args[0]), "/usr/local/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator))
		expected := map[string]string{
			"DISPLAY": ":0", "WAYLAND_DISPLAY": "wayland-0", "XDG_RUNTIME_DIR": "/run/user/1000",
			"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus", "XAUTHORITY": "/run/user/1000/xauthority",
			"XDG_CURRENT_DESKTOP": "KDE", "DESKTOP_SESSION": "plasma", "KDE_FULL_SESSION": "true",
			"KDE_SESSION_VERSION": "6", "XDG_DATA_HOME": "/home/deck/.local/share",
			"XDG_SESSION_TYPE": "wayland", "XDG_SESSION_CLASS": "user", "XDG_SESSION_DESKTOP": "KDE",
			"XDG_SESSION_ID": "5", "XDG_SEAT": "seat0", "XDG_SEAT_PATH": "/org/freedesktop/DisplayManager/Seat0",
			"XDG_SESSION_PATH": "/org/freedesktop/DisplayManager/Session1", "XDG_VTNR": "1", "XDG_MENU_PREFIX": "plasma-",
			"XDG_DATA_DIRS":   "/var/lib/flatpak/exports/share:/usr/local/share:/usr/share",
			"XDG_CONFIG_HOME": "/home/deck/.config", "XDG_CONFIG_DIRS": "/home/deck/.config/kdedefaults:/etc/xdg",
			"PATH": trustedPath,
		}
		for key, value := range expected {
			if os.Getenv(key) != value {
				os.Exit(6)
			}
		}
		if os.Getenv("DECK_SNAPSHOT_UNSAFE_TEST_ENV") != "" {
			os.Exit(7)
		}
		fmt.Print("desktop environment present")
	default:
		os.Exit(5)
	}
	os.Exit(0)
}
