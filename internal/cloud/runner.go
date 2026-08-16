// Package cloud implements the narrow protected snapshot transfer boundary.
package cloud

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/logging"
)

const (
	defaultCommandTimeout = 2 * time.Minute
	defaultOutputLimit    = 2 * 1024 * 1024
	maximumCommandTimeout = 30 * time.Minute
)

type Request struct {
	Args       []string
	Stdin      []byte
	SecretEnv  map[string]string
	SecretJSON []byte
	Timeout    time.Duration
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

type Runner interface {
	Run(context.Context, Request) (Result, error)
}

type CommandRunner struct {
	Binary         string
	ConfigPath     string
	Prefix         []string
	AllowedCommand map[string]struct{}
	MaxOutput      int
	DefaultTimeout time.Duration
}

func NewRcloneRunner(binary, configPath string) (*CommandRunner, error) {
	if !filepath.IsAbs(binary) || !filepath.IsAbs(configPath) {
		return nil, errors.New("rclone binary and config paths must be absolute")
	}
	allowed := map[string]struct{}{
		"version": {}, "config": {}, "listremotes": {}, "lsjson": {}, "copyto": {}, "deletefile": {}, "mkdir": {}, "about": {}, "obscure": {},
		"secure-config-create": {},
	}
	return &CommandRunner{
		Binary:         binary,
		ConfigPath:     configPath,
		Prefix:         []string{"--config", configPath, "--ask-password=false", "--use-json-log", "--log-level", "NOTICE"},
		AllowedCommand: allowed,
		MaxOutput:      defaultOutputLimit, DefaultTimeout: defaultCommandTimeout,
	}, nil
}

func (runner *CommandRunner) Run(ctx context.Context, request Request) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if runner == nil || !filepath.IsAbs(runner.Binary) || len(request.Args) == 0 {
		return Result{}, errors.New("cloud command runner is not configured")
	}
	if len(runner.AllowedCommand) > 0 {
		if _, allowed := runner.AllowedCommand[request.Args[0]]; !allowed {
			return Result{}, fmt.Errorf("cloud command %q is not allowlisted", request.Args[0])
		}
	}
	for _, argument := range append(append([]string(nil), runner.Prefix...), request.Args...) {
		if argument == "" || strings.ContainsRune(argument, '\x00') || strings.ContainsAny(argument, "\r\n") {
			return Result{}, errors.New("cloud command contains an unsafe argument")
		}
	}
	for key := range request.SecretEnv {
		if !allowedSecretEnvironmentKey(key) {
			return Result{}, fmt.Errorf("cloud command secret environment key %q is not allowlisted", key)
		}
	}
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = runner.DefaultTimeout
	}
	if timeout <= 0 || timeout > maximumCommandTimeout {
		return Result{}, errors.New("cloud command timeout is outside the allowed range")
	}
	maximum := runner.MaxOutput
	if maximum <= 0 {
		maximum = defaultOutputLimit
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if request.Args[0] == "secure-config-create" {
		if len(request.Args) != 1 || len(request.Stdin) != 0 || len(request.SecretJSON) == 0 {
			return Result{}, errors.New("secure cloud configuration request is malformed")
		}
		return runner.runSecureConfigCreate(commandContext, request, maximum)
	}
	if len(request.SecretJSON) != 0 {
		return Result{}, errors.New("secret JSON is accepted only for secure cloud configuration")
	}
	arguments := append(append([]string(nil), runner.Prefix...), request.Args...)
	command := exec.CommandContext(commandContext, runner.Binary, arguments...)
	command.Stdin = bytes.NewReader(request.Stdin)
	command.Env = minimalEnvironment(request.SecretEnv, filepath.Dir(runner.Binary))
	stdout := &boundedBuffer{maximum: maximum}
	stderr := &boundedBuffer{maximum: maximum}
	command.Stdout = stdout
	command.Stderr = stderr
	started := time.Now()
	err := command.Run()
	result := Result{
		Stdout: logging.RedactText(stdout.String()), Stderr: logging.RedactText(stderr.String()),
		ExitCode: 0, Duration: time.Since(started),
	}
	if stdout.overflow || stderr.overflow {
		return result, errors.New("cloud command output exceeded the configured limit")
	}
	if commandContext.Err() != nil {
		return result, fmt.Errorf("cloud command did not complete before its deadline: %w", commandContext.Err())
	}
	if err != nil {
		var exitFailure *exec.ExitError
		if errors.As(err, &exitFailure) {
			result.ExitCode = exitFailure.ExitCode()
			return result, fmt.Errorf("cloud command failed with exit code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
		}
		return result, fmt.Errorf("start cloud command: %w", err)
	}
	return result, nil
}

func allowedSecretEnvironmentKey(key string) bool {
	switch key {
	case "RCLONE_CONFIG_PASS", "RCLONE_CRYPT_PASSWORD", "RCLONE_CRYPT_PASSWORD2":
		return true
	default:
		return false
	}
}

func minimalEnvironment(secrets map[string]string, executableDirectory string) []string {
	allowedHost := []string{
		"HOME", "USER", "LOGNAME", "TMPDIR", "TMP", "TEMP", "SSL_CERT_FILE", "SSL_CERT_DIR", "SYSTEMROOT", "WINDIR",
		"DISPLAY", "WAYLAND_DISPLAY", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "XAUTHORITY",
		"XDG_CURRENT_DESKTOP", "DESKTOP_SESSION", "KDE_FULL_SESSION", "KDE_SESSION_VERSION",
		"XDG_SESSION_TYPE", "XDG_SESSION_CLASS", "XDG_SESSION_DESKTOP", "XDG_SESSION_ID",
		"XDG_SEAT", "XDG_SEAT_PATH", "XDG_SESSION_PATH", "XDG_VTNR", "XDG_MENU_PREFIX",
		"XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_CONFIG_HOME", "XDG_CONFIG_DIRS",
	}
	values := map[string]string{
		"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "TZ": "UTC",
		"PATH": strings.Join([]string{executableDirectory, "/usr/local/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator)),
	}
	for _, key := range allowedHost {
		if value, exists := os.LookupEnv(key); exists && value != "" {
			values[key] = value
		}
	}
	for key, value := range secrets {
		values[key] = value
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
