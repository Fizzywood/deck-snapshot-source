//go:build !linux

package restore

import (
	"context"
	"errors"
)

func gracefulSteamShutdown(context.Context) error {
	return errors.New("Steam quiescence is supported only on SteamOS/Linux")
}
