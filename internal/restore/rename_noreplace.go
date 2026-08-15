package restore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
)

type namespaceMutationError struct{ err error }

func (failure *namespaceMutationError) Error() string {
	return "namespace move committed but durability was not confirmed: " + failure.err.Error()
}
func (failure *namespaceMutationError) Unwrap() error { return failure.err }

func moveNoReplace(home, oldPath, newPath string, createDestinationParents bool) error {
	oldParent, err := openSecureParent(home, oldPath, false)
	if err != nil {
		return err
	}
	defer oldParent.root.Close()
	newParent, err := openSecureParent(home, newPath, createDestinationParents)
	if err != nil {
		return err
	}
	defer newParent.root.Close()
	if err := renameNoReplaceRoots(oldParent.root, oldParent.name, newParent.root, newParent.name); err != nil {
		return fmt.Errorf("move without replacement: %w", err)
	}
	if err := errors.Join(syncDirectoryRoot(oldParent.root), syncDirectoryRoot(newParent.root)); err != nil {
		return &namespaceMutationError{err: err}
	}
	return nil
}

func randomSibling(path, label string) (string, error) {
	identifier := make([]byte, 12)
	if _, err := rand.Read(identifier); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), ".deck-snapshot-"+label+"-"+hex.EncodeToString(identifier)), nil
}
