package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	runtimePasswordPlaceholder = "deck-snapshot-runtime-recovery-required"
)

var googleDesktopClientPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,240}\.apps\.googleusercontent\.com$`)

// ConnectGoogle creates the dedicated encrypted rclone configuration and runs
// a PKCE-protected system-browser/loopback OAuth flow. Google's generated
// Desktop credential is a non-confidential installed-app credential, but Deck
// Snapshot still keeps it out of URLs, process arguments, logs and plaintext
// files.
func (manager Manager) ConnectGoogle(ctx context.Context, clientID, clientCredential string, now time.Time) (status Status, err error) {
	if manager.ExpectedBaseType != "drive" {
		return Status{}, errors.New("Google connection requires the Drive backend")
	}
	if err := manager.validateConnectionInputs(clientID, clientCredential); err != nil {
		return Status{}, err
	}
	if err := manager.verifyRcloneVersion(ctx); err != nil {
		return Status{}, err
	}
	if _, statErr := os.Lstat(manager.ConfigPath); statErr == nil {
		return Status{}, errors.New("cloud configuration already exists; disconnect it before connecting again")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return Status{}, fmt.Errorf("inspect cloud configuration path: %w", statErr)
	}
	configDirectory := filepath.Dir(manager.ConfigPath)
	if err := ensurePrivateDirectory(configDirectory); err != nil {
		return Status{}, fmt.Errorf("prepare cloud configuration directory: %w", err)
	}
	created := true
	defer func() {
		if err != nil && created {
			_ = removePrivateRegularFile(manager.ConfigPath)
			_ = syncDirectory(configDirectory)
		}
	}()
	passwordInput := []byte(manager.ConfigPassword + "\n" + manager.ConfigPassword + "\n")
	defer clear(passwordInput)
	if _, err = manager.Runner.Run(ctx, Request{
		Args: []string{"config", "encryption", "set"}, Stdin: passwordInput,
		Timeout: defaultCommandTimeout,
	}); err != nil {
		return Status{}, fmt.Errorf("initialize encrypted cloud configuration: %w", err)
	}
	if err = secureAndSyncRegularFile(manager.ConfigPath); err != nil {
		return Status{}, err
	}
	token, err := manager.obtainGoogleToken(ctx, clientID, clientCredential)
	if err != nil {
		return Status{}, fmt.Errorf("complete Google Drive PKCE authorization: %w", err)
	}
	defer clear(token)
	secureInput, err := json.Marshal(secureConfigInput{
		Name: manager.BaseRemote, Type: "drive",
		Parameters: map[string]string{
			"client_id": clientID, "client_secret": clientCredential, "scope": "drive.file", "config_is_local": "true", "token": string(token),
		},
	})
	if err != nil {
		return Status{}, errors.New("prepare protected Google Drive configuration")
	}
	defer clear(secureInput)
	if _, err = manager.Runner.Run(ctx, Request{
		Args: []string{"secure-config-create"}, SecretJSON: secureInput,
		SecretEnv: map[string]string{"RCLONE_CONFIG_PASS": manager.ConfigPassword}, Timeout: defaultCommandTimeout,
	}); err != nil {
		return Status{}, fmt.Errorf("store Google authorization in the encrypted cloud configuration: %w", err)
	}
	if err = secureAndSyncRegularFile(manager.ConfigPath); err != nil {
		return Status{}, err
	}
	remote := manager.BaseRemote + ":" + manager.BasePath
	if _, err = manager.run(ctx, "config", "create", manager.CryptRemote, "crypt",
		"remote", remote,
		"password", runtimePasswordPlaceholder,
		"password2", runtimePasswordPlaceholder+"-two",
		"filename_encryption", "standard",
		"directory_name_encryption", "true",
		"--obscure", "--no-output"); err != nil {
		return Status{}, fmt.Errorf("create protected cloud wrapper: %w", err)
	}
	if err = secureAndSyncRegularFile(manager.ConfigPath); err != nil {
		return Status{}, err
	}
	profile, err := manager.preflightProfile(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("verify connected protected cloud: %w", err)
	}
	if _, err = manager.run(ctx, "mkdir", manager.remoteRoot()); err != nil {
		return Status{}, fmt.Errorf("create the protected Google Drive snapshot folder: %w", err)
	}
	if _, err = manager.listRemote(ctx); err != nil {
		return Status{}, fmt.Errorf("verify protected Google Drive reachability: %w", err)
	}
	if err = manager.AcknowledgeRecovery(now); err != nil {
		return Status{}, fmt.Errorf("record recovery acknowledgement: %w", err)
	}
	return Status{
		Configured: true, Protected: true, RecoveryAcknowledged: true, Remote: manager.CryptRemote,
		Scope: profile.scope, Folder: profile.folder, Legacy: profile.legacy,
	}, nil
}

// Disconnect forgets only Deck Snapshot's local configuration and recovery
// acknowledgement. It deliberately leaves the provider grant intact so a
// fresh installation can rediscover drive.file objects created by this app.
func (manager Manager) Disconnect(ctx context.Context) error {
	if err := manager.preflight(ctx); err != nil {
		return err
	}
	listed, err := manager.run(ctx, "listremotes")
	if err != nil {
		return fmt.Errorf("list configured cloud remotes: %w", err)
	}
	remotes := nonEmptyLines(listed.Stdout)
	sort.Strings(remotes)
	expected := []string{manager.BaseRemote + ":", manager.CryptRemote + ":"}
	sort.Strings(expected)
	if len(remotes) != len(expected) || remotes[0] != expected[0] || remotes[1] != expected[1] {
		return errors.New("cloud configuration contains unexpected remotes and was not removed")
	}
	if err := removePrivateRegularFile(manager.ConfigPath); err != nil {
		return fmt.Errorf("remove cloud configuration: %w", err)
	}
	acknowledgementPath := filepath.Join(manager.StateDirectory, "cloud", "recovery-acknowledgement.json")
	if err := removePrivateRegularFileIfExists(acknowledgementPath); err != nil {
		return fmt.Errorf("remove recovery acknowledgement: %w", err)
	}
	if err := syncDirectory(filepath.Dir(manager.ConfigPath)); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(acknowledgementPath))
}

func (manager Manager) validateConnectionInputs(clientID, clientCredential string) error {
	if err := manager.validate(); err != nil {
		return err
	}
	if !googleDesktopClientPattern.MatchString(clientID) {
		return errors.New("Google Desktop OAuth client ID is invalid")
	}
	if !validGoogleDesktopCredential(clientCredential) {
		return errors.New("Google Desktop OAuth credential is invalid")
	}
	if len(manager.ConfigPassword) < 12 || len(manager.ConfigPassword) > 1024 || strings.ContainsAny(manager.ConfigPassword, "\x00\r\n") {
		return errors.New("cloud configuration password must contain 12 to 1024 characters without line breaks")
	}
	return nil
}

func secureAndSyncRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("cloud configuration was not created as a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func removePrivateRegularFileIfExists(path string) error {
	err := removePrivateRegularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func removePrivateRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing to remove a non-regular private cloud file")
	}
	return os.Remove(path)
}

func nonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
