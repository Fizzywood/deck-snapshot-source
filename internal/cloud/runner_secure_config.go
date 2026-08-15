package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const secureConfigMaximumSteps = 8

type secureConfigInput struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Parameters map[string]string `json:"parameters"`
}

type secureConfigRCRequest struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Parameters map[string]string `json:"parameters"`
	Opt        secureConfigRCOpt `json:"opt"`
}

type secureConfigRCOpt struct {
	NonInteractive bool   `json:"nonInteractive"`
	Continue       bool   `json:"continue,omitempty"`
	State          string `json:"state,omitempty"`
	Result         string `json:"result,omitempty"`
}

type secureConfigRCResponse struct {
	State  string `json:"State"`
	Error  string `json:"Error"`
	Option struct {
		Name string `json:"Name"`
	} `json:"Option"`
}

func (runner *CommandRunner) runSecureConfigCreate(ctx context.Context, request Request, maximum int) (result Result, err error) {
	started := time.Now()
	defer func() { result.Duration = time.Since(started) }()
	if runtime.GOOS != "linux" || !filepath.IsAbs(runner.ConfigPath) {
		return result, errors.New("secure cloud configuration requires the supported Linux runtime")
	}
	input, err := validateSecureConfigInput(request.SecretJSON)
	if err != nil {
		return result, err
	}
	temporaryDirectory, err := os.MkdirTemp("", "ds-rc-")
	if err != nil {
		return result, errors.New("prepare private cloud configuration channel")
	}
	if err := os.Chmod(temporaryDirectory, 0o700); err != nil {
		_ = os.RemoveAll(temporaryDirectory)
		return result, errors.New("protect private cloud configuration channel")
	}
	cleanTemporary := func() {
		resolvedParent, parentErr := filepath.EvalSymlinks(filepath.Dir(temporaryDirectory))
		resolvedTemporary, temporaryErr := filepath.EvalSymlinks(temporaryDirectory)
		if parentErr == nil && temporaryErr == nil && filepath.Dir(resolvedTemporary) == resolvedParent && strings.HasPrefix(filepath.Base(resolvedTemporary), "ds-rc-") {
			_ = os.RemoveAll(resolvedTemporary)
		}
	}
	defer cleanTemporary()
	socketPath := filepath.Join(temporaryDirectory, "rc.sock")
	if len(socketPath) > 100 {
		return result, errors.New("private cloud configuration socket path is too long")
	}

	arguments := append(append([]string(nil), runner.Prefix...), "rcd", "--rc-addr", "unix://"+socketPath, "--rc-no-auth")
	command := exec.CommandContext(ctx, runner.Binary, arguments...)
	command.Env = minimalEnvironment(request.SecretEnv, filepath.Dir(runner.Binary))
	stdout := &boundedBuffer{maximum: maximum}
	stderr := &boundedBuffer{maximum: maximum}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return result, errors.New("start private cloud configuration helper")
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		select {
		case <-waited:
		case <-time.After(2 * time.Second):
		}
	}()
	if err := waitForPrivateSocket(ctx, socketPath, waited); err != nil {
		return result, err
	}

	transport := &http.Transport{
		DialContext: func(dialContext context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(dialContext, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	state := ""
	answer := ""
	for step := 0; step < secureConfigMaximumSteps; step++ {
		payload := secureConfigRCRequest{
			Name: input.Name, Type: input.Type, Parameters: input.Parameters,
			Opt: secureConfigRCOpt{NonInteractive: true, Continue: state != "", State: state, Result: answer},
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return result, errors.New("encode private cloud configuration request")
		}
		response, err := postPrivateConfig(ctx, client, encoded, maximum)
		clear(encoded)
		if err != nil {
			return result, err
		}
		if response.Error != "" {
			return result, errors.New("rclone rejected the private cloud configuration request")
		}
		if response.State == "" {
			if stdout.overflow || stderr.overflow {
				return result, errors.New("private cloud configuration helper output exceeded the configured limit")
			}
			return result, nil
		}
		switch response.Option.Name {
		case "config_refresh_token", "config_team_drive", "config_change_team_drive":
			answer = "false"
		default:
			return result, fmt.Errorf("rclone requested unexpected cloud configuration step %q", response.Option.Name)
		}
		if len(response.State) > 64*1024 || strings.ContainsRune(response.State, '\x00') {
			return result, errors.New("rclone cloud configuration state exceeded its safe bound")
		}
		state = response.State
	}
	return result, errors.New("rclone cloud configuration exceeded its step limit")
}

func validateSecureConfigInput(encoded []byte) (secureConfigInput, error) {
	if len(encoded) == 0 || len(encoded) > googleOAuthMaximumBody {
		return secureConfigInput{}, errors.New("secure cloud configuration input exceeded its safe bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var input secureConfigInput
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return secureConfigInput{}, errors.New("secure cloud configuration input is invalid")
	}
	if input.Type != "drive" || input.Name == "" || strings.ContainsAny(input.Name, ":/\\\x00\r\n") {
		return secureConfigInput{}, errors.New("secure cloud configuration identity is invalid")
	}
	if len(input.Parameters) != 5 || input.Parameters["scope"] != "drive.file" || input.Parameters["config_is_local"] != "true" ||
		!googleDesktopClientPattern.MatchString(input.Parameters["client_id"]) || !validGoogleDesktopCredential(input.Parameters["client_secret"]) {
		return secureConfigInput{}, errors.New("secure cloud configuration parameters are invalid")
	}
	for key := range input.Parameters {
		switch key {
		case "client_id", "client_secret", "scope", "config_is_local", "token":
		default:
			return secureConfigInput{}, errors.New("secure cloud configuration contains an unexpected parameter")
		}
	}
	var token rcloneOAuthToken
	tokenDecoder := json.NewDecoder(strings.NewReader(input.Parameters["token"]))
	tokenDecoder.DisallowUnknownFields()
	now := time.Now().UTC()
	if err := tokenDecoder.Decode(&token); err != nil || tokenDecoder.Decode(&struct{}{}) != io.EOF ||
		!validOAuthSecret(token.AccessToken, 16384) || !validOAuthSecret(token.RefreshToken, 16384) ||
		token.TokenType != "Bearer" || token.Expiry.Before(now.Add(-5*time.Minute)) || token.Expiry.After(now.Add(24*time.Hour)) {
		return secureConfigInput{}, errors.New("secure cloud configuration token is invalid")
	}
	return input, nil
}

func waitForPrivateSocket(ctx context.Context, socketPath string, waited <-chan error) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.New("private cloud configuration helper was canceled")
		case <-deadline.C:
			return errors.New("private cloud configuration helper did not become ready")
		case <-waited:
			return errors.New("private cloud configuration helper exited before becoming ready")
		case <-ticker.C:
			info, err := os.Lstat(socketPath)
			if err == nil && info.Mode()&os.ModeSocket != 0 {
				return nil
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return errors.New("private cloud configuration socket is unsafe")
			}
		}
	}
}

func postPrivateConfig(ctx context.Context, client *http.Client, encoded []byte, maximum int) (secureConfigRCResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/config/create", bytes.NewReader(encoded))
	if err != nil {
		return secureConfigRCResponse{}, errors.New("prepare private cloud configuration request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return secureConfigRCResponse{}, errors.New("private cloud configuration request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(maximum)+1))
	if err != nil || len(body) > maximum {
		return secureConfigRCResponse{}, errors.New("private cloud configuration response exceeded its safe bound")
	}
	if response.StatusCode != http.StatusOK {
		return secureConfigRCResponse{}, fmt.Errorf("private cloud configuration failed with HTTP status %d", response.StatusCode)
	}
	var value secureConfigRCResponse
	if err := json.Unmarshal(body, &value); err != nil {
		return secureConfigRCResponse{}, errors.New("private cloud configuration returned an invalid response")
	}
	return value, nil
}

func clear(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
