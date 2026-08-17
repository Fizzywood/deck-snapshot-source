package restore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	incompleteTransactionName = "restore-incomplete.json"
	maxTransactionMarkerBytes = 64 << 10
)

// incompleteTransactionMarker is deliberately limited to information needed
// to identify a recoverable transaction. It never contains snapshot payloads,
// OAuth material, or recovery secrets.
type incompleteTransactionMarker struct {
	Schema                   int      `json:"schema"`
	PlanID                   string   `json:"plan_id"`
	SnapshotID               string   `json:"snapshot_id"`
	SnapshotPath             string   `json:"snapshot_path"`
	RecoveryPath             string   `json:"recovery_path"`
	OriginalPluginInventory  []string `json:"original_plugin_inventory"`
	OriginalDisabledPlugins  []string `json:"original_disabled_plugins"`
	TemporaryDisabledPlugins []string `json:"temporary_disabled_plugins"`
	PreRebootBootID          string   `json:"pre_reboot_boot_id"`
	Phase                    string   `json:"phase"`
}

func transactionMarkerPath(state string) string {
	return filepath.Join(state, incompleteTransactionName)
}

func validateTransactionMarker(marker incompleteTransactionMarker) error {
	if marker.Schema != 2 || !safeTransactionID(marker.PlanID) || !safeTransactionID(marker.SnapshotID) || !validTransactionPhase(marker.Phase) {
		return errors.New("incomplete restore marker has an invalid identity or phase")
	}
	if !filepath.IsAbs(marker.RecoveryPath) || len(marker.RecoveryPath) > 4096 {
		return errors.New("incomplete restore marker has an unsafe recovery path")
	}
	if !filepath.IsAbs(marker.SnapshotPath) || len(marker.SnapshotPath) > 4096 {
		return errors.New("incomplete restore marker has an unsafe snapshot path")
	}
	if !validBootID(marker.PreRebootBootID) {
		return errors.New("incomplete restore marker has an invalid boot identity")
	}
	for _, values := range [][]string{marker.OriginalPluginInventory, marker.OriginalDisabledPlugins, marker.TemporaryDisabledPlugins} {
		if len(values) > 512 {
			return errors.New("incomplete restore marker has too many disabled plugins")
		}
		seen := make(map[string]struct{}, len(values))
		for _, name := range values {
			if !safePluginIdentity(name) {
				return errors.New("incomplete restore marker has an unsafe plugin identity")
			}
			if _, duplicate := seen[name]; duplicate {
				return errors.New("incomplete restore marker has duplicate plugin identities")
			}
			seen[name] = struct{}{}
		}
	}
	return nil
}

func validTransactionPhase(phase string) bool {
	switch phase {
	case "prepared", "plugins_quiesced", "plugin_convergence", "steam_quiesced", "filesystem_convergence", "static_verification_passed", "awaiting_reboot", "awaiting_post_boot_verification", "completed", "recovery_required":
		return true
	default:
		return false
	}
}

func safeTransactionID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func safePluginIdentity(value string) bool {
	if len(value) == 0 || len(value) > 128 || value != strings.TrimSpace(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func saveIncompleteTransaction(home, state string, marker incompleteTransactionMarker) error {
	if err := validateTransactionMarker(marker); err != nil {
		return err
	}
	if err := ensureSecureDirectory(home, state); err != nil {
		return err
	}
	path := transactionMarkerPath(state)
	parent, err := openSecureParent(home, path, false)
	if err != nil {
		return err
	}
	defer parent.root.Close()
	contents, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	if len(contents) > maxTransactionMarkerBytes {
		return errors.New("incomplete restore marker exceeds the size limit")
	}
	if _, err := parent.root.Lstat(parent.name); err == nil {
		return errors.New("an incomplete restore transaction already requires recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporaryName := ".restore-incomplete-" + hex.EncodeToString(random) + ".tmp"
	temporary, err := parent.root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = temporary.Close()
		if remove {
			_ = parent.root.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := renameNoReplaceRoots(parent.root, temporaryName, parent.root, parent.name); err != nil {
		return err
	}
	remove = false
	return nil
}

// updateIncompleteTransaction atomically exchanges a validated marker with a
// newly fsynced copy, retaining the previous marker until the new namespace is
// durable. It is available only on platforms with the same atomic-exchange
// guarantee as transactional restore.
func updateIncompleteTransaction(home, state string, marker incompleteTransactionMarker) error {
	if err := validateTransactionMarker(marker); err != nil {
		return err
	}
	path := transactionMarkerPath(state)
	parent, err := openSecureParent(home, path, false)
	if err != nil {
		return err
	}
	defer parent.root.Close()
	info, err := parent.root.Lstat(parent.name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || isLinkOrReparsePoint(info) || !privateMarkerMode(info) {
		return errors.New("incomplete restore marker is unsafe")
	}
	contents, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	if len(contents) > maxTransactionMarkerBytes {
		return errors.New("incomplete restore marker exceeds the size limit")
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporaryName := ".restore-incomplete-" + hex.EncodeToString(random) + ".tmp"
	temporary, err := parent.root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = temporary.Close()
		if remove {
			_ = parent.root.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := exchangeRoots(parent.root, temporaryName, parent.root, parent.name); err != nil {
		return err
	}
	if err := syncDirectoryRoot(parent.root); err != nil {
		return err
	}
	old, err := parent.root.Lstat(temporaryName)
	if err != nil || !old.Mode().IsRegular() || isLinkOrReparsePoint(old) {
		return errors.New("previous incomplete restore marker changed during update")
	}
	if err := parent.root.Remove(temporaryName); err != nil {
		return err
	}
	if err := syncDirectoryRoot(parent.root); err != nil {
		return err
	}
	remove = false
	return nil
}

func loadIncompleteTransaction(home, state string) (incompleteTransactionMarker, bool, error) {
	path := transactionMarkerPath(state)
	parent, err := openSecureParent(home, path, false)
	if errors.Is(err, os.ErrNotExist) {
		return incompleteTransactionMarker{}, false, nil
	}
	if err != nil {
		return incompleteTransactionMarker{}, false, err
	}
	defer parent.root.Close()
	info, err := parent.root.Lstat(parent.name)
	if errors.Is(err, os.ErrNotExist) {
		return incompleteTransactionMarker{}, false, nil
	}
	if err != nil {
		return incompleteTransactionMarker{}, false, err
	}
	if !info.Mode().IsRegular() || isLinkOrReparsePoint(info) || !privateMarkerMode(info) || info.Size() > maxTransactionMarkerBytes {
		return incompleteTransactionMarker{}, false, errors.New("incomplete restore marker is unsafe")
	}
	file, err := parent.root.Open(parent.name)
	if err != nil {
		return incompleteTransactionMarker{}, false, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxTransactionMarkerBytes+1))
	decoder.DisallowUnknownFields()
	var marker incompleteTransactionMarker
	if err := decoder.Decode(&marker); err != nil {
		return incompleteTransactionMarker{}, false, fmt.Errorf("decode incomplete restore marker: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return incompleteTransactionMarker{}, false, errors.New("incomplete restore marker contains trailing data")
	}
	if err := validateTransactionMarker(marker); err != nil {
		return incompleteTransactionMarker{}, false, err
	}
	sort.Strings(marker.OriginalPluginInventory)
	sort.Strings(marker.OriginalDisabledPlugins)
	sort.Strings(marker.TemporaryDisabledPlugins)
	return marker, true, nil
}

func removeIncompleteTransaction(home, state string) error {
	path := transactionMarkerPath(state)
	parent, err := openSecureParent(home, path, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.root.Close()
	info, err := parent.root.Lstat(parent.name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || isLinkOrReparsePoint(info) || !privateMarkerMode(info) {
		return errors.New("incomplete restore marker is unsafe")
	}
	return parent.root.Remove(parent.name)
}
