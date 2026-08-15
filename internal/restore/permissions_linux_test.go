//go:build linux

package restore

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
)

func TestBuildPlanRejectsUnwritableRestoreRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses ordinary directory permission checks")
	}
	created, _ := fixtureSnapshot(t)
	target := targetPaths(t)
	if err := os.MkdirAll(target.Decky, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target.Decky, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(target.Decky, 0o700)
	resolver := staticResolver{url: "https://example.test/package.zip", hash: strings.Repeat("a", 64)}
	_, err := BuildPlan(context.Background(), PlanOptions{Paths: target, SnapshotPath: created.Path, AppVersion: "phase3-test", Now: time.Now(), Limits: limits.Default(), Resolver: resolver})
	if err == nil {
		t.Fatal("BuildPlan accepted an unwritable restore root")
	}
}
