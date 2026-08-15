package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	recoverySchema      = 1
	maximumRecoverySize = 16 * 1024
)

// RecoveryMaterial is the separately stored key material required to decrypt
// protected cloud snapshots after a fresh installation. It never contains an
// OAuth token or the rclone configuration password.
type RecoveryMaterial struct {
	Schema         int    `json:"schema"`
	CreatedUTC     string `json:"created_utc"`
	CryptPassword  string `json:"crypt_password"`
	CryptPassword2 string `json:"crypt_password_2"`
}

type ProtectedRecovery struct {
	Password            string
	Password2           string
	MaterialFingerprint string
}

func GenerateRecovery(now time.Time) (RecoveryMaterial, error) {
	password, err := randomRecoverySecret()
	if err != nil {
		return RecoveryMaterial{}, err
	}
	password2, err := randomRecoverySecret()
	if err != nil {
		return RecoveryMaterial{}, err
	}
	return RecoveryMaterial{
		Schema: recoverySchema, CreatedUTC: now.UTC().Format(time.RFC3339Nano),
		CryptPassword: password, CryptPassword2: password2,
	}, nil
}

func SaveRecovery(path string, material RecoveryMaterial) error {
	if err := validateRecovery(material); err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil || absolute != filepath.Clean(path) {
		return errors.New("recovery export path must be an absolute clean path")
	}
	directory := filepath.Dir(absolute)
	if err := validateExistingDirectory(directory); err != nil {
		return errors.New("recovery export directory is missing or unsafe")
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("recovery export directory is missing or unsafe")
	}
	encoded, err := json.MarshalIndent(material, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(absolute, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("recovery export already exists; it was not replaced")
		}
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(absolute)
		}
	}()
	if _, err := file.Write(encoded); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return syncDirectory(directory)
}

func LoadRecovery(path string) (RecoveryMaterial, error) {
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Clean(path) != absolute || validateExistingDirectory(filepath.Dir(absolute)) != nil {
		return RecoveryMaterial{}, errors.New("recovery material path is unsafe")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || privateFileModeError(info) != nil {
		return RecoveryMaterial{}, errors.New("recovery material is missing or not a private regular file")
	}
	if info.Size() <= 0 || info.Size() > maximumRecoverySize {
		return RecoveryMaterial{}, errors.New("recovery material size is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return RecoveryMaterial{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maximumRecoverySize+1))
	decoder.DisallowUnknownFields()
	var material RecoveryMaterial
	if err := decoder.Decode(&material); err != nil {
		return RecoveryMaterial{}, errors.New("recovery material is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RecoveryMaterial{}, errors.New("recovery material has trailing data")
	}
	if err := validateRecovery(material); err != nil {
		return RecoveryMaterial{}, err
	}
	return material, nil
}

func ProtectRecovery(ctx context.Context, runner Runner, material RecoveryMaterial) (ProtectedRecovery, error) {
	if runner == nil {
		return ProtectedRecovery{}, errors.New("cloud command runner is not configured")
	}
	if err := validateRecovery(material); err != nil {
		return ProtectedRecovery{}, err
	}
	password, err := obscure(ctx, runner, material.CryptPassword)
	if err != nil {
		return ProtectedRecovery{}, fmt.Errorf("protect primary recovery secret: %w", err)
	}
	password2, err := obscure(ctx, runner, material.CryptPassword2)
	if err != nil {
		return ProtectedRecovery{}, fmt.Errorf("protect secondary recovery secret: %w", err)
	}
	fingerprint, err := RecoveryFingerprint(material)
	if err != nil {
		return ProtectedRecovery{}, err
	}
	return ProtectedRecovery{Password: password, Password2: password2, MaterialFingerprint: fingerprint}, nil
}

func RecoveryFingerprint(material RecoveryMaterial) (string, error) {
	if err := validateRecovery(material); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(material)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func obscure(ctx context.Context, runner Runner, secret string) (string, error) {
	result, err := runner.Run(ctx, Request{Args: []string{"obscure", "-"}, Stdin: []byte(secret + "\n"), Timeout: 30 * time.Second})
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(result.Stdout)
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\r\n \t") {
		return "", errors.New("rclone returned invalid protected recovery material")
	}
	return value, nil
}

func validateRecovery(material RecoveryMaterial) error {
	if material.Schema != recoverySchema {
		return errors.New("recovery material uses an unsupported schema")
	}
	if _, err := time.Parse(time.RFC3339Nano, material.CreatedUTC); err != nil {
		return errors.New("recovery material creation time is invalid")
	}
	if !validRecoverySecret(material.CryptPassword) || !validRecoverySecret(material.CryptPassword2) || material.CryptPassword == material.CryptPassword2 {
		return errors.New("recovery material contains invalid crypt secrets")
	}
	return nil
}

func validRecoverySecret(value string) bool {
	if len(value) < 40 || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n \t") {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) >= 32
}

func randomRecoverySecret() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
