//go:build !linux

package restore

import (
	"context"
	"errors"
)

type sessionRebooter struct{}

func (sessionRebooter) Preflight(context.Context) error {
	return errors.New("session-authorized reboot is supported only on SteamOS/Linux")
}
func (sessionRebooter) Request(context.Context) error {
	return errors.New("session-authorized reboot is supported only on SteamOS/Linux")
}
func (sessionRebooter) BootID(context.Context) (string, error) {
	return "", errors.New("system boot identity is supported only on SteamOS/Linux")
}
