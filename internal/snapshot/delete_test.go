package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Fizzywood/deck-snapshot/internal/limits"
)

func TestDeleteValidatedRemovesOnlyOneExactValidatedSnapshot(t *testing.T) {
	directory := t.TempDir()
	created, err := Create(context.Background(), directory, discoverFixture(t), limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(created.Path)
	if err := DeleteValidated(context.Background(), directory, name, limits.Default()); err != nil {
		t.Fatalf("DeleteValidated() = %v", err)
	}
	if _, err := os.Lstat(created.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected snapshot still exists: %v", err)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("DeleteValidated() affected unexpected entries: %#v, %v", entries, err)
	}
}

func TestDeleteValidatedRejectsPathsAndNonSnapshots(t *testing.T) {
	directory := t.TempDir()
	created, err := Create(context.Background(), directory, discoverFixture(t), limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(created.Path)
	for _, unsafe := range []string{"../" + name, created.Path, "recovery.tar.gz", "report.txt", "*"} {
		if err := DeleteValidated(context.Background(), directory, unsafe, limits.Default()); err == nil {
			t.Fatalf("DeleteValidated accepted %q", unsafe)
		}
	}
	if _, err := os.Lstat(created.Path); err != nil {
		t.Fatalf("unsafe request changed the real snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "report.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DeleteValidated(context.Background(), directory, "report.txt", limits.Default()); err == nil {
		t.Fatal("DeleteValidated accepted a non-snapshot report")
	}
}

func TestDeleteValidatedRejectsLinks(t *testing.T) {
	directory := t.TempDir()
	created, err := Create(context.Background(), directory, discoverFixture(t), limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(created.Path)
	if err := os.Remove(created.Path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.tar.gz")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, name)); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := DeleteValidated(context.Background(), directory, name, limits.Default()); err == nil {
		t.Fatal("DeleteValidated accepted a symlink")
	}
	if _, err := os.Lstat(target); err != nil {
		t.Fatalf("symlink rejection changed outside target: %v", err)
	}
}
