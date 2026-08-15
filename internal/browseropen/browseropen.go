// Package browseropen implements the constrained system-browser adapter used
// directly by Deck Snapshot and by the retained rclone xdg-open boundary.
package browseropen

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	usageExitCode   = 64
	failureExitCode = 69
	queryLimit      = 4096
	queryTimeout    = 5 * time.Second
)

type dependencies struct {
	lookupEnv    func(string) (string, bool)
	stat         func(string) (os.FileInfo, error)
	queryDefault func([]string) (string, error)
	launch       func(string, string, []string) error
}

// Run validates one OAuth authorization URL, resolves the user's registered
// HTTPS handler, and opens it without exposing cloud-secret environment fields.
// It deliberately emits no output because the URL contains transient OAuth data.
func Run(args []string) int {
	return run(args, systemDependencies())
}

// OpenAuthorizationURL opens one validated Google OAuth authorization URL in
// the registered system browser without inheriting cloud-secret environment
// fields. Errors deliberately contain no URL or transient OAuth state.
func OpenAuthorizationURL(value string) error {
	switch code := run([]string{value}, systemDependencies()); code {
	case 0:
		return nil
	case usageExitCode:
		return errors.New("authorization URL was rejected")
	default:
		return errors.New("system browser could not be opened")
	}
}

func systemDependencies() dependencies {
	deps := dependencies{
		lookupEnv: os.LookupEnv,
		stat:      os.Stat,
	}
	deps.queryDefault = func(environment []string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
		defer cancel()
		command := exec.CommandContext(ctx, "/usr/bin/xdg-mime", "query", "default", "x-scheme-handler/https")
		command.Env = environment
		output := &boundedBuffer{maximum: queryLimit}
		command.Stdout = output
		command.Stderr = &boundedBuffer{maximum: queryLimit}
		if err := command.Run(); err != nil {
			return "", err
		}
		if output.overflow {
			return "", errors.New("default browser response exceeded limit")
		}
		return strings.TrimSpace(output.String()), nil
	}
	deps.launch = func(desktopFile, targetURL string, environment []string) error {
		command := exec.Command("/usr/bin/gio", "launch", desktopFile, targetURL)
		command.Env = environment
		return command.Run()
	}
	return deps
}

func run(args []string, deps dependencies) int {
	if len(args) != 1 || !allowedAuthorizationURL(args[0]) {
		return usageExitCode
	}
	environment := safeEnvironment(deps.lookupEnv)
	desktop, err := deps.queryDefault(environment)
	if err != nil || !validDesktopID(desktop) {
		return failureExitCode
	}
	desktopFile, err := resolveDesktopFile(desktop, deps.lookupEnv, deps.stat)
	if err != nil {
		return failureExitCode
	}
	if err := deps.launch(desktopFile, args[0], environment); err != nil {
		return failureExitCode
	}
	return 0
}

func allowedAuthorizationURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if parsed.Scheme == "https" && parsed.Hostname() == "accounts.google.com" && parsed.Port() == "" {
		return true
	}
	if parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Path != "/auth" {
		return false
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1024 || port > 65535 {
		return false
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(query) != 1 || len(query["state"]) != 1 {
		return false
	}
	state := query["state"][0]
	if len(state) < 16 || len(state) > 256 {
		return false
	}
	for _, character := range state {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validDesktopID(value string) bool {
	if value == "" || strings.ContainsAny(value, "/\\\x00\r\n") || !strings.HasSuffix(value, ".desktop") {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("._+-", character) {
			continue
		}
		return false
	}
	return true
}

func resolveDesktopFile(desktop string, lookupEnv func(string) (string, bool), stat func(string) (os.FileInfo, error)) (string, error) {
	home, _ := lookupEnv("HOME")
	dataHome, exists := lookupEnv("XDG_DATA_HOME")
	if !exists || dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	dataDirectories, exists := lookupEnv("XDG_DATA_DIRS")
	if !exists || dataDirectories == "" {
		dataDirectories = strings.Join([]string{"/usr/local/share", "/usr/share"}, string(os.PathListSeparator))
	}
	search := append([]string{dataHome}, filepath.SplitList(dataDirectories)...)
	for _, directory := range search {
		if !filepath.IsAbs(directory) || strings.ContainsAny(directory, "\x00\r\n") {
			continue
		}
		candidate := filepath.Join(directory, "applications", desktop)
		info, err := stat(candidate)
		if err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", errors.New("default browser desktop file was not found")
}

func safeEnvironment(lookupEnv func(string) (string, bool)) []string {
	allowedHost := []string{
		"HOME", "USER", "LOGNAME", "TMPDIR", "TMP", "TEMP", "SSL_CERT_FILE", "SSL_CERT_DIR",
		"DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "XAUTHORITY",
		"XDG_CURRENT_DESKTOP", "DESKTOP_SESSION", "KDE_FULL_SESSION", "KDE_SESSION_VERSION",
		"XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS",
	}
	values := map[string]string{
		"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "TZ": "UTC", "PATH": "/usr/local/bin:/usr/bin:/bin",
	}
	for _, key := range allowedHost {
		if value, exists := lookupEnv(key); exists && value != "" {
			values[key] = value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

type boundedBuffer struct {
	buffer   bytes.Buffer
	maximum  int
	overflow bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	requested := len(value)
	remaining := buffer.maximum - buffer.buffer.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		_, _ = buffer.buffer.Write(value)
	}
	if requested > remaining {
		buffer.overflow = true
	}
	return requested, nil
}

func (buffer *boundedBuffer) String() string { return buffer.buffer.String() }
