//go:build linux

package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	busctlBinary          = "/usr/bin/busctl"
	logindService         = "org.freedesktop.login1"
	logindObject          = "/org/freedesktop/login1"
	logindManager         = "org.freedesktop.login1.Manager"
	canRebootMethod       = "CanReboot"
	rebootMethod          = "Reboot"
	logindResponseMaxSize = 128
)

// logindClient intentionally exposes only the two fixed Manager methods used
// by restore. No caller can select a D-Bus destination, method, or argument.
type logindClient interface {
	CanReboot(context.Context) (string, error)
	Reboot(context.Context, bool) error
}

type sessionRebooter struct{ logind logindClient }

func (rebooter sessionRebooter) Preflight(ctx context.Context) error {
	capability, err := rebooter.client().CanReboot(ctx)
	if err != nil {
		return errors.New("logind reboot capability is unavailable")
	}
	switch capability {
	case "yes", "challenge":
		return nil
	case "no", "na":
		return errors.New("a session-authorized Steam Deck reboot is unavailable")
	default:
		return errors.New("logind returned an invalid reboot capability")
	}
}

func (rebooter sessionRebooter) Request(ctx context.Context) error {
	// interactive=true preserves logind's normal authorization path for the
	// documented "challenge" capability without bypassing inhibitors or Polkit.
	if err := rebooter.client().Reboot(ctx, true); err != nil {
		return errors.New("the session-authorized Steam Deck reboot request failed")
	}
	return nil
}

func (rebooter sessionRebooter) client() logindClient {
	if rebooter.logind != nil {
		return rebooter.logind
	}
	return systemLogindClient{}
}

func (sessionRebooter) BootID(context.Context) (string, error) {
	file, err := os.Open("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", errors.New("read system boot identity")
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, 64))
	value := strings.TrimSpace(string(contents))
	if err != nil || len(contents) > 63 || !validBootID(value) {
		return "", errors.New("read system boot identity")
	}
	return value, nil
}

type systemLogindClient struct{}

func (systemLogindClient) CanReboot(ctx context.Context) (string, error) {
	output, err := logindCall(ctx, canRebootMethod)
	if err != nil {
		return "", err
	}
	return parseLogindString(output)
}

func (systemLogindClient) Reboot(ctx context.Context, interactive bool) error {
	argument := "false"
	if interactive {
		argument = "true"
	}
	_, err := logindCall(ctx, rebootMethod, "b", argument)
	return err
}

func logindCall(ctx context.Context, method string, signatureAndArguments ...string) ([]byte, error) {
	info, err := os.Stat(busctlBinary)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("system D-Bus client is unavailable")
	}
	arguments := []string{"--system", "call", logindService, logindObject, logindManager, method}
	arguments = append(arguments, signatureAndArguments...)
	commandCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, busctlBinary, arguments...)
	output, err := command.Output()
	if len(output) > logindResponseMaxSize {
		return nil, errors.New("logind returned an oversized response")
	}
	if err != nil {
		return nil, fmt.Errorf("call fixed logind D-Bus method: %w", err)
	}
	return output, err
}

func parseLogindString(output []byte) (string, error) {
	parts := strings.Fields(strings.TrimSpace(string(output)))
	if len(parts) != 2 || parts[0] != "s" {
		return "", errors.New("invalid logind string response")
	}
	value, err := strconv.Unquote(parts[1])
	if err != nil || value == "" || len(value) > 16 {
		return "", errors.New("invalid logind string response")
	}
	return value, nil
}
