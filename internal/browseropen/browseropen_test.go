package browseropen

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunAllowsOnlyGoogleAndScrubsSecrets(t *testing.T) {
	root := t.TempDir()
	desktop := "org.mozilla.firefox.desktop"
	desktopPath := filepath.Join(root, "applications", desktop)
	if err := os.MkdirAll(filepath.Dir(desktopPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desktopPath, []byte("[Desktop Entry]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{
		"HOME": root, "XDG_DATA_HOME": root, "DISPLAY": ":0",
		"RCLONE_CONFIG_PASS": "must-not-pass", "RCLONE_DRIVE_CLIENT_SECRET": "must-not-pass",
	}
	lookup := func(key string) (string, bool) {
		value, exists := environment[key]
		return value, exists
	}
	launched := false
	deps := dependencies{
		lookupEnv: lookup,
		stat:      os.Stat,
		queryDefault: func(received []string) (string, error) {
			assertSafeEnvironment(t, received)
			return desktop, nil
		},
		launch: func(receivedDesktop, receivedURL string, received []string) error {
			assertSafeEnvironment(t, received)
			if receivedDesktop != desktopPath || receivedURL != "https://accounts.google.com/o/oauth2/auth?state=test" {
				t.Fatalf("unexpected browser launch target: %q %q", receivedDesktop, receivedURL)
			}
			launched = true
			return nil
		},
	}
	if code := run([]string{"https://accounts.google.com/o/oauth2/auth?state=test"}, deps); code != 0 || !launched {
		t.Fatalf("run() = %d, launched=%v", code, launched)
	}
	launched = false
	deps.launch = func(receivedDesktop, receivedURL string, received []string) error {
		assertSafeEnvironment(t, received)
		if receivedDesktop != desktopPath || receivedURL != "http://127.0.0.1:53682/auth?state=abcdefghijklmnopqrstuvwxyz012345" {
			t.Fatalf("unexpected loopback launch target: %q %q", receivedDesktop, receivedURL)
		}
		launched = true
		return nil
	}
	if code := run([]string{"http://127.0.0.1:53682/auth?state=abcdefghijklmnopqrstuvwxyz012345"}, deps); code != 0 || !launched {
		t.Fatalf("run(loopback) = %d, launched=%v", code, launched)
	}
	for _, value := range []string{
		"http://accounts.google.com/o/oauth2/auth",
		"https://accounts.google.com.evil.example/o/oauth2/auth",
		"https://accounts.google.com:443/o/oauth2/auth",
		"https://user@accounts.google.com/o/oauth2/auth",
		"http://127.0.0.1/auth?state=abcdefghijklmnopqrstuvwxyz012345",
		"http://127.0.0.1:80/auth?state=abcdefghijklmnopqrstuvwxyz012345",
		"http://localhost:53682/auth?state=abcdefghijklmnopqrstuvwxyz012345",
		"http://127.0.0.1:53682/other?state=abcdefghijklmnopqrstuvwxyz012345",
		"http://127.0.0.1:53682/auth?state=too-short",
		"http://127.0.0.1:53682/auth?state=abcdefghijklmnopqrstuvwxyz012345&extra=value",
		"https://example.com/",
	} {
		if code := run([]string{value}, deps); code != usageExitCode {
			t.Fatalf("run(%q) = %d", value, code)
		}
	}
	if code := run(nil, deps); code != usageExitCode {
		t.Fatalf("run(nil) = %d", code)
	}
	if code := run([]string{"https://accounts.google.com/test", "extra"}, deps); code != usageExitCode {
		t.Fatalf("run(extra argument) = %d", code)
	}
}

func TestRunRejectsUnsafeDesktopResponsesAndFailures(t *testing.T) {
	base := dependencies{
		lookupEnv: func(string) (string, bool) { return "", false },
		stat:      os.Stat,
		launch:    func(string, string, []string) error { return nil },
	}
	for _, desktop := range []string{"", "../firefox.desktop", "firefox", "firefox desktop.desktop", "firefox.desktop\nother"} {
		deps := base
		deps.queryDefault = func([]string) (string, error) { return desktop, nil }
		if code := run([]string{"https://accounts.google.com/test"}, deps); code != failureExitCode {
			t.Fatalf("run() accepted desktop %q with code %d", desktop, code)
		}
	}
	deps := base
	deps.queryDefault = func([]string) (string, error) { return "", errors.New("failed") }
	if code := run([]string{"https://accounts.google.com/test"}, deps); code != failureExitCode {
		t.Fatalf("run() query failure = %d", code)
	}
}

func assertSafeEnvironment(t *testing.T, environment []string) {
	t.Helper()
	joined := strings.Join(environment, "\n")
	if strings.Contains(joined, "RCLONE_CONFIG_PASS") || strings.Contains(joined, "RCLONE_DRIVE_CLIENT_SECRET") || !strings.Contains(joined, "DISPLAY=:0") {
		t.Fatalf("unsafe browser environment: %q", environment)
	}
}
