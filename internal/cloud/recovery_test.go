package cloud

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type obscureRunner struct{ inputs []string }

func (runner *obscureRunner) Run(_ context.Context, request Request) (Result, error) {
	if len(request.Args) != 2 || request.Args[0] != "obscure" || request.Args[1] != "-" {
		return Result{}, errors.New("unexpected command")
	}
	runner.inputs = append(runner.inputs, strings.TrimSpace(string(request.Stdin)))
	return Result{Stdout: "obscured-" + string(rune('a'+len(runner.inputs))) + "\n"}, nil
}

func TestRecoveryExportLoadProtectAndFingerprint(t *testing.T) {
	material, err := GenerateRecovery(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deck-snapshot-recovery.json")
	if err := SaveRecovery(path, material); err != nil {
		t.Fatal(err)
	}
	if err := SaveRecovery(path, material); err == nil || !strings.Contains(err.Error(), "not replaced") {
		t.Fatalf("SaveRecovery() replaced an export: %v", err)
	}
	loaded, err := LoadRecovery(path)
	if err != nil || loaded != material {
		t.Fatalf("LoadRecovery() = %#v, %v", loaded, err)
	}
	runner := &obscureRunner{}
	protected, err := ProtectRecovery(context.Background(), runner, loaded)
	if err != nil || len(runner.inputs) != 2 || runner.inputs[0] != material.CryptPassword || runner.inputs[1] != material.CryptPassword2 {
		t.Fatalf("ProtectRecovery() = %#v, inputs=%d, %v", protected, len(runner.inputs), err)
	}
	expected, err := RecoveryFingerprint(material)
	if err != nil || protected.MaterialFingerprint != expected || !validFingerprint(expected) {
		t.Fatalf("unexpected recovery fingerprint: %q, %v", protected.MaterialFingerprint, err)
	}
}

func TestRecoveryRejectsTrailingData(t *testing.T) {
	material, err := GenerateRecovery(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "recovery.json")
	if err := SaveRecovery(path, material); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("{}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRecovery(path); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("LoadRecovery() accepted trailing data: %v", err)
	}
}

func TestManagedRecoveryIsIdempotentButNeverReplaced(t *testing.T) {
	material, err := GenerateRecovery(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	other, err := GenerateRecovery(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "recovery.json")
	if err := SaveManagedRecovery(path, material); err != nil {
		t.Fatal(err)
	}
	if err := SaveManagedRecovery(path, material); err != nil {
		t.Fatalf("identical managed recovery was not accepted: %v", err)
	}
	if err := SaveManagedRecovery(path, other); err == nil || !strings.Contains(err.Error(), "different material") {
		t.Fatalf("conflicting managed recovery was accepted: %v", err)
	}
	loaded, err := LoadRecovery(path)
	if err != nil || loaded != material {
		t.Fatalf("managed recovery changed after conflict: %#v, %v", loaded, err)
	}
}
