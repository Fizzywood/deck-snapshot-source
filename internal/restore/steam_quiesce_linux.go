//go:build linux

package restore

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const steamShutdownBinary = "/usr/bin/steam"

// gracefulSteamShutdown uses Steam's fixed shutdown argument rather than a
// shell or a signal. It refuses to proceed until the bounded process names
// that own supported Steam artwork state are absent.
func gracefulSteamShutdown(ctx context.Context) error {
	info, err := os.Stat(steamShutdownBinary)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("the supported Steam shutdown command is unavailable")
	}
	command := exec.CommandContext(ctx, steamShutdownBinary, "-shutdown")
	if err := command.Run(); err != nil {
		return errors.New("Steam did not accept the controlled shutdown request")
	}
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		present, err := steamArtworkWritersPresent()
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("Steam artwork writers did not stop safely")
		case <-ticker.C:
		}
	}
}

func steamArtworkWritersPresent() (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, errors.New("inspect Steam process state")
	}
	if len(entries) > 1<<16 {
		return false, errors.New("Steam process state is unexpectedly large")
	}
	for _, entry := range entries {
		if !entry.IsDir() || !allDigits(entry.Name()) {
			continue
		}
		file, readErr := os.Open(filepath.Join("/proc", entry.Name(), "comm"))
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return false, errors.New("inspect Steam process identity")
		}
		contents, readErr := io.ReadAll(io.LimitReader(file, 129))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || len(contents) > 128 {
			return false, errors.New("inspect Steam process identity")
		}
		name := strings.TrimSpace(string(contents))
		if name == "steam" || name == "steamwebhelper" {
			return true, nil
		}
	}
	return false, nil
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
