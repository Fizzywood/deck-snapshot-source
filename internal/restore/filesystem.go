package restore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type openedParent struct {
	root *os.Root
	name string
}

func openSecureParent(home, target string, create bool) (openedParent, error) {
	relative, err := relativeToHome(home, target)
	if err != nil {
		return openedParent{}, err
	}
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	if len(parts) < 2 || parts[len(parts)-1] == "" || parts[len(parts)-1] == "." || parts[len(parts)-1] == ".." {
		return openedParent{}, errors.New("target has no safe parent or filename")
	}
	homeInfo, err := os.Lstat(home)
	if err != nil || !homeInfo.IsDir() || isLinkOrReparsePoint(homeInfo) {
		return openedParent{}, errors.New("target home is not a real directory")
	}
	if err := platformOwnedByCurrentUser(homeInfo); err != nil {
		return openedParent{}, fmt.Errorf("target home ownership is unsafe: %w", err)
	}
	current, err := os.OpenRoot(home)
	if err != nil {
		return openedParent{}, fmt.Errorf("open target home root: %w", err)
	}
	for _, component := range parts[:len(parts)-1] {
		if component == "" || component == "." || component == ".." {
			current.Close()
			return openedParent{}, errors.New("target contains an unsafe path component")
		}
		info, statErr := current.Lstat(component)
		if errors.Is(statErr, os.ErrNotExist) && create {
			if err := current.Mkdir(component, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				current.Close()
				return openedParent{}, fmt.Errorf("create restore directory %q: %w", component, err)
			}
			info, statErr = current.Lstat(component)
		}
		if statErr != nil {
			current.Close()
			return openedParent{}, fmt.Errorf("inspect restore directory %q: %w", component, statErr)
		}
		if isLinkOrReparsePoint(info) || !info.IsDir() {
			current.Close()
			return openedParent{}, fmt.Errorf("restore directory is not a real directory: %q", component)
		}
		if err := platformOwnedByCurrentUser(info); err != nil {
			current.Close()
			return openedParent{}, fmt.Errorf("restore directory ownership is unsafe: %q: %w", component, err)
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			current.Close()
			return openedParent{}, fmt.Errorf("open restore directory %q: %w", component, err)
		}
		current.Close()
		current = next
	}
	return openedParent{root: current, name: parts[len(parts)-1]}, nil
}

func ensureSecureDirectory(home, directory string) error {
	parent, err := openSecureParent(home, filepath.Join(directory, ".deck-snapshot-directory-scope"), true)
	if err != nil {
		return err
	}
	defer parent.root.Close()
	return nil
}

type privateDirectory struct {
	Path   string
	name   string
	parent *os.Root
	root   *os.Root
	retain bool
}

func createPrivateDirectory(home, parentPath, prefix string) (*privateDirectory, error) {
	if err := ensureSecureDirectory(home, parentPath); err != nil {
		return nil, err
	}
	parent, err := openSecureParent(home, filepath.Join(parentPath, ".deck-snapshot-private-scope"), false)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 16; attempt++ {
		identifier := make([]byte, 12)
		if _, err := rand.Read(identifier); err != nil {
			parent.root.Close()
			return nil, err
		}
		name := prefix + hex.EncodeToString(identifier)
		if err := parent.root.Mkdir(name, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			parent.root.Close()
			return nil, err
		}
		root, err := parent.root.OpenRoot(name)
		if err != nil {
			_ = parent.root.Remove(name)
			parent.root.Close()
			return nil, err
		}
		if err := syncDirectoryRoot(parent.root); err != nil {
			root.Close()
			parent.root.Close()
			return nil, fmt.Errorf("durably create private restore directory: %w", err)
		}
		return &privateDirectory{Path: filepath.Join(parentPath, name), name: name, parent: parent.root, root: root}, nil
	}
	parent.root.Close()
	return nil, errors.New("unable to allocate a unique private restore directory")
}

func (directory *privateDirectory) Retain() {
	if directory != nil {
		directory.retain = true
	}
}

func (directory *privateDirectory) Close() error {
	if directory == nil {
		return nil
	}
	if directory.retain {
		rootErr := directory.root.Close()
		parentErr := directory.parent.Close()
		return errors.Join(rootErr, parentErr)
	}
	opened, openErr := directory.root.Open(".")
	var entries []os.DirEntry
	var readErr error
	if openErr == nil {
		entries, readErr = opened.ReadDir(-1)
		_ = opened.Close()
	} else {
		readErr = openErr
	}
	var failures []error
	if readErr != nil {
		failures = append(failures, readErr)
	} else {
		for _, entry := range entries {
			if err := directory.root.RemoveAll(entry.Name()); err != nil {
				failures = append(failures, err)
			}
		}
	}
	if err := directory.root.Close(); err != nil {
		failures = append(failures, err)
	}
	if len(failures) == 0 {
		if err := directory.parent.Remove(directory.name); err != nil {
			failures = append(failures, err)
		} else if err := syncDirectoryRoot(directory.parent); err != nil {
			failures = append(failures, err)
		}
	}
	if err := directory.parent.Close(); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func inspectParentFile(parent openedParent, maximum int64) (exists bool, size int64, hash string, mode uint32, err error) {
	before, err := parent.root.Lstat(parent.name)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, "", 0, nil
	}
	if err != nil {
		return false, 0, "", 0, err
	}
	if !before.Mode().IsRegular() || isLinkOrReparsePoint(before) || before.Size() < 0 || before.Size() > maximum {
		return false, 0, "", 0, errors.New("target is not a bounded regular file")
	}
	if err := platformOwnedByCurrentUser(before); err != nil {
		return false, 0, "", 0, err
	}
	file, err := parent.root.Open(parent.name)
	if err != nil {
		return false, 0, "", 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return false, 0, "", 0, errors.New("target identity changed while opening")
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, maximum+1))
	if err != nil || written != opened.Size() {
		return false, 0, "", 0, errors.New("target changed while hashing")
	}
	after, err := parent.root.Lstat(parent.name)
	if err != nil || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return false, 0, "", 0, errors.New("target changed while hashing")
	}
	return true, opened.Size(), hex.EncodeToString(digest.Sum(nil)), uint32(opened.Mode().Perm()), nil
}

type incompleteMutationError struct{ err error }

func (failure *incompleteMutationError) Error() string {
	return "restore mutation could not be conclusively rolled back: " + failure.err.Error()
}
func (failure *incompleteMutationError) Unwrap() error { return failure.err }

func inspectFileAt(home, path string, maximum int64) (bool, int64, string, uint32, error) {
	parent, err := openSecureParent(home, path, false)
	if err != nil {
		return false, 0, "", 0, err
	}
	defer parent.root.Close()
	return inspectParentFile(parent, maximum)
}

func atomicWrite(ctx context.Context, home string, action Action, sourcePath string, mode os.FileMode, expectedOperation string) (resultErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parent, err := openSecureParent(home, action.TargetPath, true)
	if err != nil {
		return err
	}
	defer parent.root.Close()
	exists, size, hash, existingMode, err := inspectParentFile(parent, maxInt64(action.Size, action.ExistingSize))
	if err != nil {
		return err
	}
	switch expectedOperation {
	case "create":
		if exists {
			return errors.New("restore target appeared after approval")
		}
	case "replace":
		if !exists || size != action.ExistingSize || hash != action.ExistingSHA256 || existingMode != action.ExistingMode {
			return errors.New("restore target changed after approval")
		}
	default:
		return errors.New("unsupported atomic write operation")
	}

	transaction, err := createPrivateDirectory(home, filepath.Dir(action.TargetPath), ".deck-snapshot-file-transaction-")
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := transaction.Close(); resultErr == nil && closeErr != nil {
			resultErr = &incompleteMutationError{err: errors.Join(fmt.Errorf("clean private file transaction: %w", closeErr), fmt.Errorf("private transaction path: %s", transaction.Path))}
		}
	}()

	const temporaryName = "payload"
	temporary, err := transaction.root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	defer temporary.Close()
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	digest := sha256.New()
	written, copyErr := copyRestoreContext(ctx, io.MultiWriter(temporary, digest), io.LimitReader(source, action.Size+1))
	closeSourceErr := source.Close()
	if copyErr != nil || closeSourceErr != nil || written != action.Size || hex.EncodeToString(digest.Sum(nil)) != action.SHA256 {
		return errors.New("staged restore payload failed verification during write")
	}
	if err := temporary.Chmod(mode.Perm()); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := syncDirectoryRoot(transaction.root); err != nil {
		return fmt.Errorf("durably stage restore payload: %w", err)
	}
	if expectedOperation == "replace" {
		if err := exchangeRoots(transaction.root, temporaryName, parent.root, parent.name); err != nil {
			return fmt.Errorf("atomically exchange restore target: %w", err)
		}
		if err := syncNamespaceRoots(transaction.root, parent.root); err != nil {
			return retainIncomplete(transaction, fmt.Errorf("durably commit restore target exchange: %w", err))
		}
		displacedExists, displacedSize, displacedHash, displacedMode, inspectErr := inspectParentFile(openedParent{root: transaction.root, name: temporaryName}, maxInt64(action.Size, action.ExistingSize))
		if inspectErr != nil || !displacedExists || displacedSize != action.ExistingSize || displacedHash != action.ExistingSHA256 || displacedMode != action.ExistingMode {
			exchangeBackErr := exchangeRoots(transaction.root, temporaryName, parent.root, parent.name)
			syncErr := error(nil)
			if exchangeBackErr == nil {
				syncErr = syncNamespaceRoots(transaction.root, parent.root)
			}
			if exchangeBackErr != nil || syncErr != nil {
				return retainIncomplete(transaction, errors.Join(errors.New("displaced target did not match the approved identity"), inspectErr, exchangeBackErr, syncErr))
			}
			reversedExists, reversedSize, reversedHash, reversedMode, reversedErr := inspectParentFile(openedParent{root: transaction.root, name: temporaryName}, action.Size)
			if reversedErr != nil || !reversedExists || reversedSize != action.Size || reversedHash != action.SHA256 || !appliedModeMatches(reversedMode, uint32(mode.Perm())) {
				return retainIncomplete(transaction, errors.Join(errors.New("restore target changed during reversal; the private object was retained"), inspectErr, reversedErr))
			}
			return errors.Join(errors.New("restore target changed during atomic exchange; the exchange was reversed"), inspectErr)
		}
	} else {
		if err := renameNoReplaceRoots(transaction.root, temporaryName, parent.root, parent.name); err != nil {
			return fmt.Errorf("publish restore target without replacement: %w", err)
		}
		if err := syncNamespaceRoots(transaction.root, parent.root); err != nil {
			return retainIncomplete(transaction, fmt.Errorf("durably commit created restore target: %w", err))
		}
	}
	exists, size, hash, appliedMode, err := inspectParentFile(parent, action.Size)
	if err != nil || !exists || size != action.Size || hash != action.SHA256 || !appliedModeMatches(appliedMode, uint32(mode.Perm())) {
		if expectedOperation == "replace" {
			exchangeBackErr := exchangeRoots(transaction.root, temporaryName, parent.root, parent.name)
			syncErr := error(nil)
			if exchangeBackErr == nil {
				syncErr = syncNamespaceRoots(transaction.root, parent.root)
			}
			if exchangeBackErr != nil || syncErr != nil {
				return retainIncomplete(transaction, errors.Join(errors.New("published restore target failed verification"), err, exchangeBackErr, syncErr))
			}
			failedExists, failedSize, failedHash, failedMode, failedErr := inspectParentFile(openedParent{root: transaction.root, name: temporaryName}, maxInt64(action.Size, 1))
			if failedErr == nil && failedExists && failedSize == action.Size && failedHash == action.SHA256 && appliedModeMatches(failedMode, uint32(mode.Perm())) {
				return errors.New("published restore target failed verification; the atomic exchange was reversed")
			}
			return retainIncomplete(transaction, errors.Join(errors.New("published restore target changed concurrently; the displaced content was retained"), err, failedErr))
		}
		moveErr := renameNoReplaceRoots(parent.root, parent.name, transaction.root, "failed")
		syncErr := error(nil)
		if moveErr == nil {
			syncErr = syncNamespaceRoots(parent.root, transaction.root)
		}
		if moveErr != nil || syncErr != nil {
			return retainIncomplete(transaction, errors.Join(errors.New("published restore target failed verification"), err, moveErr, syncErr))
		}
		failedExists, failedSize, failedHash, failedMode, failedErr := inspectParentFile(openedParent{root: transaction.root, name: "failed"}, maxInt64(action.Size, 1))
		if failedErr == nil && failedExists && failedSize == action.Size && failedHash == action.SHA256 && appliedModeMatches(failedMode, uint32(mode.Perm())) {
			return errors.New("published restore target failed verification; the created payload was quarantined")
		}
		return retainIncomplete(transaction, errors.Join(errors.New("published created target changed concurrently"), err, failedErr))
	}
	if expectedOperation == "replace" {
		backupExists, backupSize, backupHash, backupMode, backupErr := inspectParentFile(openedParent{root: transaction.root, name: temporaryName}, action.ExistingSize)
		if backupErr != nil || !backupExists || backupSize != action.ExistingSize || backupHash != action.ExistingSHA256 || backupMode != action.ExistingMode {
			return retainIncomplete(transaction, errors.Join(errors.New("the preserved pre-write target changed before cleanup"), backupErr))
		}
		if err := transaction.root.Remove(temporaryName); err != nil {
			return retainIncomplete(transaction, fmt.Errorf("remove verified private pre-write target: %w", err))
		}
		if err := syncDirectoryRoot(transaction.root); err != nil {
			return retainIncomplete(transaction, fmt.Errorf("durably remove verified private pre-write target: %w", err))
		}
	}
	return nil
}

func removeAppliedCreate(home string, action Action) (resultErr error) {
	parent, err := openSecureParent(home, action.TargetPath, false)
	if err != nil {
		return err
	}
	defer parent.root.Close()
	exists, size, hash, mode, err := inspectParentFile(parent, action.Size)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	expectedMode := action.ExistingMode
	if expectedMode == 0 {
		expectedMode = action.DesiredMode
	}
	if size != action.Size || hash != action.SHA256 || mode != expectedMode {
		return errors.New("removal target changed after approval")
	}
	transaction, err := createPrivateDirectory(home, filepath.Dir(action.TargetPath), ".deck-snapshot-file-rollback-")
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := transaction.Close(); resultErr == nil && closeErr != nil {
			resultErr = fmt.Errorf("clean private file rollback: %w", closeErr)
		}
	}()
	if err := renameNoReplaceRoots(parent.root, parent.name, transaction.root, "removed"); err != nil {
		return err
	}
	if err := syncNamespaceRoots(parent.root, transaction.root); err != nil {
		return retainIncomplete(transaction, fmt.Errorf("durably quarantine rollback target: %w", err))
	}
	movedExists, movedSize, movedHash, movedMode, inspectErr := inspectParentFile(openedParent{root: transaction.root, name: "removed"}, action.Size)
	if inspectErr != nil || !movedExists || movedSize != action.Size || movedHash != action.SHA256 {
		restoreErr := restoreMovedRegularRoots(transaction.root, "removed", parent.root, parent.name, movedSize, movedHash, movedMode)
		return retainIncomplete(transaction, errors.Join(errors.New("rollback target identity changed during removal"), inspectErr, restoreErr))
	}
	if err := transaction.root.Remove("removed"); err != nil {
		return retainIncomplete(transaction, fmt.Errorf("remove verified rollback target: %w", err))
	}
	if err := syncDirectoryRoot(transaction.root); err != nil {
		return retainIncomplete(transaction, fmt.Errorf("durably remove verified rollback target: %w", err))
	}
	return nil
}

func restoreMovedRegularRoots(sourceRoot *os.Root, sourceName string, targetRoot *os.Root, targetName string, size int64, hash string, mode uint32) error {
	if err := renameNoReplaceRoots(sourceRoot, sourceName, targetRoot, targetName); err != nil {
		return err
	}
	if err := syncNamespaceRoots(sourceRoot, targetRoot); err != nil {
		return err
	}
	exists, restoredSize, restoredHash, restoredMode, err := inspectParentFile(openedParent{root: targetRoot, name: targetName}, maxInt64(size, 1))
	if err != nil || !exists || restoredSize != size || restoredHash != hash || (mode != 0 && restoredMode != mode) {
		return errors.Join(errors.New("restored moved file failed identity verification"), err)
	}
	return nil
}

func retainIncomplete(directory *privateDirectory, failure error) error {
	directory.Retain()
	return &incompleteMutationError{err: errors.Join(failure, fmt.Errorf("retained private transaction at %s", directory.Path))}
}

func syncNamespaceRoots(first, second *os.Root) error {
	if first == second {
		return syncDirectoryRoot(first)
	}
	return errors.Join(syncDirectoryRoot(first), syncDirectoryRoot(second))
}

func copyRestoreContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func maxInt64(first, second int64) int64 {
	if first > second {
		return first
	}
	return second
}
