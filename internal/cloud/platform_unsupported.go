//go:build !linux

package cloud

import "errors"

func validateCloudPlatform() error {
	return errors.New("protected cloud execution is currently supported only on Linux")
}
