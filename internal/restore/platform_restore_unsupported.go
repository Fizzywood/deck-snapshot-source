//go:build !linux

package restore

import "errors"

func validatePlatformRestoreSupport() error {
	return errors.New("transactional restore is currently supported only on Linux; this platform cannot guarantee crash-safe atomic exchange")
}
