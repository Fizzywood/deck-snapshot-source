package restore

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

func restoreDeckySettingsSideEffects(ctx context.Context, home string, guard *LoaderSettingsGuard, recovery map[string]snapshot.StagedFile, resourceLimits limits.Limits) error {
	if guard == nil {
		return nil
	}
	exists, size, checksum, mode, err := inspectFileAt(home, guard.TargetPath, resourceLimits.MaxFileSize)
	if err != nil {
		return fmt.Errorf("inspect Decky Loader settings side effect: %w", err)
	}
	switch guard.Operation {
	case "remove":
		if !exists {
			return nil
		}
		dynamic := Action{
			LogicalPath: deckyLoaderRecoveryLogicalPath,
			TargetRoot:  guard.TargetRoot,
			TargetPath:  guard.TargetPath,
			Operation:   "create",
			Size:        size,
			SHA256:      checksum,
			DesiredMode: mode,
		}
		if err := removeAppliedCreate(home, dynamic); err != nil {
			return fmt.Errorf("remove Decky Loader settings side effect: %w", err)
		}
		return nil
	case "restore":
		payload, copied := recovery[deckyLoaderRecoveryLogicalPath]
		if !copied || payload.Size != guard.ExistingSize || payload.SHA256 != guard.ExistingSHA256 {
			return errors.New("validated Decky Loader settings recovery payload is missing")
		}
		restore := Action{
			LogicalPath: deckyLoaderRecoveryLogicalPath,
			TargetRoot:  guard.TargetRoot,
			TargetPath:  guard.TargetPath,
			Size:        guard.ExistingSize,
			SHA256:      guard.ExistingSHA256,
			DesiredMode: guard.ExistingMode,
		}
		if !exists {
			restore.Operation = "create"
			if err := atomicWrite(ctx, home, restore, payload.Path, os.FileMode(guard.ExistingMode), "create"); err != nil {
				return fmt.Errorf("recreate Decky Loader settings side-effect target: %w", err)
			}
			return nil
		}
		if size == guard.ExistingSize && checksum == guard.ExistingSHA256 && mode == guard.ExistingMode {
			return nil
		}
		restore.Operation = "replace"
		restore.ExistingSize = size
		restore.ExistingSHA256 = checksum
		restore.ExistingMode = mode
		if err := atomicWrite(ctx, home, restore, payload.Path, os.FileMode(guard.ExistingMode), "replace"); err != nil {
			return fmt.Errorf("restore Decky Loader settings side-effect target: %w", err)
		}
		return nil
	default:
		return errors.New("Decky Loader settings recovery guard operation is invalid")
	}
}
