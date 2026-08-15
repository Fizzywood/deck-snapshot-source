package snapshot

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/discovery"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
)

func TestCreateValidateAndListFixture(t *testing.T) {
	result := discoverFixture(t)
	directory := t.TempDir()
	created, err := Create(context.Background(), directory, result, limits.Default())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	validated, err := Validate(created.Path, limits.Default())
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if validated.SnapshotID != result.Manifest.SnapshotID || created.Size <= 0 {
		t.Fatalf("unexpected creation result: %#v", created)
	}
	listed, err := List(directory, limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || !listed[0].Valid || listed[0].SnapshotID != result.Manifest.SnapshotID {
		t.Fatalf("List() = %#v", listed)
	}
}

func TestCreateIsDeterministic(t *testing.T) {
	result := discoverFixture(t)
	first, err := Create(context.Background(), t.TempDir(), result, limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Create(context.Background(), t.TempDir(), result, limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, _ := os.ReadFile(first.Path)
	secondBytes, _ := os.ReadFile(second.Path)
	if string(firstBytes) != string(secondBytes) {
		t.Fatal("identical fixture inputs did not produce identical archives")
	}
}

func TestCreateNeverOverwritesExistingSnapshot(t *testing.T) {
	result := discoverFixture(t)
	directory := t.TempDir()
	first, err := Create(context.Background(), directory, result, limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(context.Background(), directory, result, limits.Default()); err == nil {
		t.Fatal("Create() overwrote an existing snapshot identity")
	}
	after, err := os.ReadFile(first.Path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("existing snapshot changed after collision: %v", err)
	}
}

func TestCreateRemovesTemporaryFileWhenSourceChecksumChanges(t *testing.T) {
	result := discoverFixture(t)
	for index := range result.Candidates {
		if !result.Candidates[index].Entry.Generated {
			result.Candidates[index].Entry.SHA256 = strings.Repeat("0", 64)
			for fileIndex := range result.Manifest.Files {
				if result.Manifest.Files[fileIndex].LogicalPath == result.Candidates[index].Entry.LogicalPath {
					result.Manifest.Files[fileIndex].SHA256 = result.Candidates[index].Entry.SHA256
				}
			}
			break
		}
	}
	directory := t.TempDir()
	if _, err := Create(context.Background(), directory, result, limits.Default()); err == nil {
		t.Fatal("Create() accepted a changed source checksum")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("Create() exposed a partial file: %#v", entries)
	}
}

func TestCreateRemovesTemporaryFileWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	directory := t.TempDir()
	if _, err := Create(ctx, directory, discoverFixture(t), limits.Default()); err == nil {
		t.Fatal("Create() accepted a cancelled operation")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled Create() exposed a partial file: %#v", entries)
	}
}

func TestValidateRejectsTruncatedArchive(t *testing.T) {
	created, err := Create(context.Background(), t.TempDir(), discoverFixture(t), limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(created.Path)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(t.TempDir(), "truncated.tar.gz")
	if err := os.WriteFile(truncated, contents[:len(contents)-8], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(truncated, limits.Default()); err == nil {
		t.Fatal("Validate() accepted a truncated archive")
	}
}

func TestValidateRejectsTraversalAndDuplicateEntries(t *testing.T) {
	base := manifest.New("dsnap-test", "test", "device", "", time.Now())
	traversal := filepath.Join(t.TempDir(), "traversal.tar.gz")
	writeRawArchive(t, traversal, base, []rawEntry{{name: "../escape", data: []byte("x")}})
	if _, err := Validate(traversal, limits.Default()); err == nil {
		t.Fatal("Validate() accepted traversal")
	}

	data := []byte("safe")
	hash := sha256.Sum256(data)
	base.Files = []manifest.File{{LogicalPath: "reports/safe.txt", Component: "reports", Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:]), Mode: 0o600}}
	duplicate := filepath.Join(t.TempDir(), "duplicate.tar.gz")
	writeRawArchive(t, duplicate, base, []rawEntry{{name: "reports/safe.txt", data: data}, {name: "reports/safe.txt", data: data}})
	if _, err := Validate(duplicate, limits.Default()); err == nil {
		t.Fatal("Validate() accepted a duplicate archive entry")
	}
}

func TestValidateRejectsUndeclaredChecksumMismatchAndHardlink(t *testing.T) {
	base := manifest.New("dsnap-test", "test", "device", "", time.Now())
	undeclared := filepath.Join(t.TempDir(), "undeclared.tar.gz")
	writeRawArchive(t, undeclared, base, []rawEntry{{name: "reports/undeclared.txt", data: []byte("x")}})
	if _, err := Validate(undeclared, limits.Default()); err == nil {
		t.Fatal("Validate() accepted an undeclared entry")
	}

	expected := []byte("expected")
	hash := sha256.Sum256(expected)
	base.Files = []manifest.File{{LogicalPath: "reports/value.txt", Component: "reports", Size: int64(len(expected)), SHA256: hex.EncodeToString(hash[:]), Mode: 0o600}}
	mismatch := filepath.Join(t.TempDir(), "mismatch.tar.gz")
	writeRawArchive(t, mismatch, base, []rawEntry{{name: "reports/value.txt", data: []byte("modified")}})
	if _, err := Validate(mismatch, limits.Default()); err == nil {
		t.Fatal("Validate() accepted a checksum mismatch")
	}

	base.Files = nil
	hardlink := filepath.Join(t.TempDir(), "hardlink.tar.gz")
	writeRawArchive(t, hardlink, base, []rawEntry{{name: "reports/link", typeflag: tar.TypeLink}})
	if _, err := Validate(hardlink, limits.Default()); err == nil {
		t.Fatal("Validate() accepted a hardlink entry")
	}
}

func TestValidateEnforcesCompressionRatio(t *testing.T) {
	created, err := Create(context.Background(), t.TempDir(), discoverFixture(t), limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	restricted := limits.Default()
	restricted.MaxCompressionRatio = 1
	if _, err := Validate(created.Path, restricted); err == nil {
		t.Fatal("Validate() ignored the compression-ratio limit")
	}
}

func TestValidateRejectsDecodedPaddingAfterTarTerminator(t *testing.T) {
	created, err := Create(context.Background(), t.TempDir(), discoverFixture(t), limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(created.Path)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := gzip.NewReader(source)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(compressed)
	if err != nil {
		t.Fatal(err)
	}
	_ = compressed.Close()
	_ = source.Close()
	decoded = append(decoded, bytes.Repeat([]byte{0}, 2<<20)...)
	padded := filepath.Join(t.TempDir(), "padded.tar.gz")
	output, err := os.Create(padded)
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(output)
	if _, err := writer.Write(decoded); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(padded, limits.Default()); err == nil {
		t.Fatal("Validate() accepted decoded padding after the tar terminator")
	}
}

func TestValidateContextHonorsCancellation(t *testing.T) {
	created, err := Create(context.Background(), t.TempDir(), discoverFixture(t), limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ValidateContext(ctx, created.Path, limits.Default()); !errors.Is(err, context.Canceled) {
		t.Fatalf("ValidateContext() error = %v, want context cancellation", err)
	}
}

func TestValidateRejectsUnknownMajorVersion(t *testing.T) {
	value := manifest.New("dsnap-test", "test", "device", "", time.Now())
	value.FormatVersion = "2.0"
	path := filepath.Join(t.TempDir(), "unknown.tar.gz")
	writeRawArchive(t, path, value, nil)
	if _, err := Validate(path, limits.Default()); err == nil {
		t.Fatal("Validate() accepted an unknown major version")
	}
}

type rawEntry struct {
	name     string
	data     []byte
	typeflag byte
}

func writeRawArchive(t *testing.T, path string, value manifest.Manifest, entries []rawEntry) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	manifestBytes, _ := json.Marshal(value)
	if err := tarWriter.WriteHeader(&tar.Header{Name: manifestPath, Mode: 0o600, Size: int64(len(manifestBytes))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(manifestBytes); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		if err := tarWriter.WriteHeader(&tar.Header{Name: entry.name, Mode: 0o600, Size: int64(len(entry.data)), Typeflag: typeflag}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func discoverFixture(t *testing.T) discovery.Result {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "tests", "fixtures", "deck-home"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := discovery.Discover(context.Background(), discovery.Options{
		Paths:      platform.Paths{Home: root, Decky: filepath.Join(root, "homebrew"), Steam: filepath.Join(root, ".local", "share", "Steam")},
		AppVersion: "phase2-test",
		DeviceID:   "ds-00000000000000000000000000000000",
		SnapshotID: "dsnap-0000000000000000",
		Now:        time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC),
		Limits:     limits.Default(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
