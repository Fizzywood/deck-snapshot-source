package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/discovery"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

type memoryRunner struct {
	mu              sync.Mutex
	objects         map[string][]byte
	unsafe          bool
	corruptDownload bool
	failUpload      bool
	failList        bool
	retainDeleted   bool
	deleteRequests  [][]string
}

func (runner *memoryRunner) Run(_ context.Context, request Request) (Result, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(request.Args) == 0 {
		return Result{}, errors.New("missing command")
	}
	switch request.Args[0] {
	case "version":
		return Result{Stdout: expectedRcloneVersion + "\n"}, nil
	case "config":
		if len(request.Args) >= 3 && request.Args[1] == "encryption" && request.Args[2] == "check" {
			return Result{}, nil
		}
		if len(request.Args) == 3 && request.Args[1] == "redacted" {
			switch request.Args[2] {
			case "deck-snapshot-crypt":
				if runner.unsafe {
					return Result{Stdout: "[deck-snapshot-crypt]\ntype = local\n"}, nil
				}
				return Result{Stdout: "[deck-snapshot-crypt]\ntype = crypt\nremote = deck-snapshot-drive:\nfilename_encryption = standard\ndirectory_name_encryption = true\npassword = XXX\n"}, nil
			case "deck-snapshot-drive":
				return Result{Stdout: "[deck-snapshot-drive]\ntype = local\n"}, nil
			}
		}
	case "copyto":
		if len(request.Args) < 3 {
			return Result{}, errors.New("copyto arguments missing")
		}
		source, destination := request.Args[1], request.Args[2]
		if strings.HasPrefix(source, "deck-snapshot-crypt:") {
			data, exists := runner.objects[strings.TrimPrefix(source, "deck-snapshot-crypt:")]
			if !exists {
				return Result{}, errors.New("remote object missing")
			}
			if _, err := os.Lstat(destination); err == nil {
				return Result{}, errors.New("immutable local destination exists")
			}
			if runner.corruptDownload {
				data = []byte("corrupted protected download")
			}
			return Result{}, os.WriteFile(destination, data, 0o600)
		}
		if runner.failUpload {
			return Result{}, context.Canceled
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return Result{}, err
		}
		name := strings.TrimPrefix(destination, "deck-snapshot-crypt:")
		if _, exists := runner.objects[name]; exists {
			return Result{}, errors.New("immutable remote destination exists")
		}
		runner.objects[name] = append([]byte(nil), data...)
		return Result{}, nil
	case "lsjson":
		if runner.failList {
			return Result{}, errors.New("synthetic remote authorization failure")
		}
		values := make([]map[string]any, 0, len(runner.objects)+1)
		for name, data := range runner.objects {
			values = append(values, map[string]any{"Name": name, "Size": len(data), "ModTime": "2026-08-14T12:00:00Z", "IsDir": false})
		}
		values = append(values, map[string]any{"Name": "../unsafe", "Size": 1, "IsDir": false})
		encoded, _ := json.Marshal(values)
		return Result{Stdout: string(encoded)}, nil
	case "deletefile":
		if len(request.Args) != 2 || !strings.HasPrefix(request.Args[1], "deck-snapshot-crypt:") {
			return Result{}, errors.New("unsafe deletefile request")
		}
		runner.deleteRequests = append(runner.deleteRequests, append([]string(nil), request.Args...))
		name := strings.TrimPrefix(request.Args[1], "deck-snapshot-crypt:")
		if _, exists := runner.objects[name]; !exists {
			return Result{}, errors.New("remote object missing")
		}
		if !runner.retainDeleted {
			delete(runner.objects, name)
		}
		return Result{}, nil
	}
	return Result{}, fmt.Errorf("unexpected command: %v", request.Args)
}

func TestManagerInspectDoesNotPublishCloudOnlySnapshot(t *testing.T) {
	created := createCloudTestSnapshot(t)
	runner := &memoryRunner{objects: make(map[string][]byte)}
	manager := cloudTestManager(t, runner)
	if err := manager.AcknowledgeRecovery(time.Now()); err != nil {
		t.Fatal(err)
	}
	uploaded, err := manager.Upload(context.Background(), created.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(created.Path); err != nil {
		t.Fatal(err)
	}
	value, item, err := manager.Inspect(context.Background(), uploaded.Name)
	if err != nil || value.SnapshotID == "" || item.Name != uploaded.Name || item.Size <= 0 {
		t.Fatalf("Inspect() = %#v, %#v, %v", value, item, err)
	}
	if _, err := os.Lstat(filepath.Join(manager.SnapshotDirectory, uploaded.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Inspect published a normal local snapshot: %v", err)
	}
}

func TestManagerTrashUsesOnlyExactSingleFileAndVerifiesListing(t *testing.T) {
	created := createCloudTestSnapshot(t)
	runner := &memoryRunner{objects: make(map[string][]byte)}
	manager := cloudTestManager(t, runner)
	if err := manager.AcknowledgeRecovery(time.Now()); err != nil {
		t.Fatal(err)
	}
	uploaded, err := manager.Upload(context.Background(), created.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Trash(context.Background(), uploaded.Name); err != nil {
		t.Fatalf("Trash() = %v", err)
	}
	if len(runner.deleteRequests) != 1 || len(runner.deleteRequests[0]) != 2 || runner.deleteRequests[0][0] != "deletefile" || runner.deleteRequests[0][1] != "deck-snapshot-crypt:"+uploaded.Name {
		t.Fatalf("Trash issued an unsafe rclone request: %#v", runner.deleteRequests)
	}
	if _, exists := runner.objects[uploaded.Name]; exists {
		t.Fatal("Trash left selected snapshot active")
	}
	for _, unsafe := range []string{"../" + uploaded.Name, "*", "recovery.json"} {
		if err := manager.Trash(context.Background(), unsafe); err == nil {
			t.Fatalf("Trash accepted %q", unsafe)
		}
	}
	if len(runner.deleteRequests) != 1 {
		t.Fatalf("unsafe Trash request reached rclone: %#v", runner.deleteRequests)
	}
}

func TestManagerTrashFailsWhenSelectedSnapshotRemainsActive(t *testing.T) {
	created := createCloudTestSnapshot(t)
	runner := &memoryRunner{objects: make(map[string][]byte), retainDeleted: true}
	manager := cloudTestManager(t, runner)
	if err := manager.AcknowledgeRecovery(time.Now()); err != nil {
		t.Fatal(err)
	}
	uploaded, err := manager.Upload(context.Background(), created.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Trash(context.Background(), uploaded.Name); err == nil || !strings.Contains(err.Error(), "still appears") {
		t.Fatalf("Trash accepted an active selected snapshot: %v", err)
	}
}

func TestManagerProtectedUploadListDownloadRoundtrip(t *testing.T) {
	created := createCloudTestSnapshot(t)
	runner := &memoryRunner{objects: make(map[string][]byte)}
	manager := cloudTestManager(t, runner)
	if _, err := manager.Upload(context.Background(), created.Path); err == nil || !strings.Contains(err.Error(), "acknowledged") {
		t.Fatalf("Upload() bypassed recovery acknowledgement: %v", err)
	}
	if err := manager.AcknowledgeRecovery(time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Check(context.Background())
	if err != nil || !status.Configured || !status.Protected || !status.RecoveryAcknowledged {
		t.Fatalf("Check() = %#v, %v", status, err)
	}
	uploaded, err := manager.Upload(context.Background(), created.Path)
	if err != nil || uploaded.Name != filepath.Base(created.Path) {
		t.Fatalf("Upload() = %#v, %v", uploaded, err)
	}
	if _, err := manager.Upload(context.Background(), created.Path); err == nil {
		t.Fatal("Upload() overwrote an immutable remote object")
	}
	items, err := manager.List(context.Background())
	if err != nil || len(items) != 1 || items[0].Name != uploaded.Name {
		t.Fatalf("List() = %#v, %v", items, err)
	}
	if err := os.Remove(created.Path); err != nil {
		t.Fatal(err)
	}
	downloaded, err := manager.Download(context.Background(), uploaded.Name)
	if err != nil || downloaded != filepath.Join(manager.SnapshotDirectory, uploaded.Name) {
		t.Fatalf("Download() = %q, %v", downloaded, err)
	}
	if _, err := snapshot.Validate(downloaded, limits.Default()); err != nil {
		t.Fatalf("downloaded snapshot is invalid: %v", err)
	}
	if _, err := manager.Download(context.Background(), uploaded.Name); err == nil {
		t.Fatal("Download() overwrote an existing local snapshot")
	}
}

func TestManagerRejectsPlaintextRemoteAndChangedAcknowledgement(t *testing.T) {
	runner := &memoryRunner{objects: make(map[string][]byte), unsafe: true}
	manager := cloudTestManager(t, runner)
	if _, err := manager.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "crypt wrapper") {
		t.Fatalf("Check() accepted unsafe remote: %v", err)
	}
	runner.unsafe = false
	if err := manager.AcknowledgeRecovery(time.Now()); err != nil {
		t.Fatal(err)
	}
	manager.ProtectionFingerprint = strings.Repeat("b", 64)
	status, err := manager.Check(context.Background())
	if err != nil || status.RecoveryAcknowledged {
		t.Fatalf("changed recovery material remained acknowledged: %#v, %v", status, err)
	}
}

func TestManagerRejectsCorruptDownloadAndAbortedUpload(t *testing.T) {
	created := createCloudTestSnapshot(t)
	runner := &memoryRunner{objects: make(map[string][]byte)}
	manager := cloudTestManager(t, runner)
	if err := manager.AcknowledgeRecovery(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Upload(context.Background(), created.Path); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(created.Path)
	runner.corruptDownload = true
	if _, err := manager.Download(context.Background(), name); err == nil || !strings.Contains(err.Error(), "validate") {
		t.Fatalf("Download() accepted corrupt content: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(manager.SnapshotDirectory, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("corrupt download was published: %v", err)
	}

	secondRunner := &memoryRunner{objects: make(map[string][]byte), failUpload: true}
	secondManager := cloudTestManager(t, secondRunner)
	if err := secondManager.AcknowledgeRecovery(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := secondManager.Upload(context.Background(), created.Path); err == nil {
		t.Fatal("Upload() reported success after an aborted transfer")
	}
	if len(secondRunner.objects) != 0 {
		t.Fatal("aborted upload published a remote object")
	}
}

func TestManagerReportsMissingConnection(t *testing.T) {
	runner := &memoryRunner{objects: make(map[string][]byte)}
	manager := cloudTestManager(t, runner)
	if err := os.Remove(manager.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Check() accepted a missing connection: %v", err)
	}
}

func TestManagerStatusRequiresRemoteReachability(t *testing.T) {
	runner := &memoryRunner{objects: make(map[string][]byte), failList: true}
	manager := cloudTestManager(t, runner)
	status, err := manager.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "contact protected cloud storage") || status.Configured || status.ConfigurationMessage == "" {
		t.Fatalf("Check() accepted an unreachable remote: %#v, %v", status, err)
	}
	local, err := manager.InspectConfiguration(context.Background())
	if err != nil || !local.Configured || !local.Protected {
		t.Fatalf("InspectConfiguration() did not preserve safe local disconnect access: %#v, %v", local, err)
	}
}

func TestRcloneLocalCryptRoundtrip(t *testing.T) {
	binary := os.Getenv("DECK_SNAPSHOT_RCLONE")
	if binary == "" {
		t.Skip("DECK_SNAPSHOT_RCLONE is not configured")
	}
	absoluteBinary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	runner, err := NewRcloneRunner(absoluteBinary, configPath)
	if err != nil {
		t.Fatal(err)
	}
	obscured, err := runner.Run(context.Background(), Request{Args: []string{"obscure", "-"}, Stdin: []byte("synthetic-local-crypt-password\n")})
	if err != nil {
		t.Fatal(err)
	}
	obscured2, err := runner.Run(context.Background(), Request{Args: []string{"obscure", "-"}, Stdin: []byte("synthetic-local-crypt-password-two\n")})
	if err != nil {
		t.Fatal(err)
	}
	remoteDirectory := filepath.ToSlash(filepath.Join(root, "encrypted-remote"))
	config := fmt.Sprintf("[deck-snapshot-drive]\ntype = local\n\n[deck-snapshot-crypt]\ntype = crypt\nremote = deck-snapshot-drive:%s\npassword = unused-runtime-placeholder\nfilename_encryption = standard\ndirectory_name_encryption = true\n", remoteDirectory)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := Manager{
		Runner: runner, SnapshotDirectory: filepath.Join(root, "snapshots"), StateDirectory: filepath.Join(root, "state"), ConfigPath: configPath,
		CryptRemote: "deck-snapshot-crypt", BaseRemote: "deck-snapshot-drive", BasePath: remoteDirectory, ExpectedBaseType: "local",
		ProtectionFingerprint: strings.Repeat("c", 64), CryptPassword: strings.TrimSpace(obscured.Stdout), CryptPassword2: strings.TrimSpace(obscured2.Stdout),
		AllowUnencryptedTest: true, Limits: limits.Default(),
	}
	created := createCloudTestSnapshot(t)
	if err := manager.AcknowledgeRecovery(time.Now()); err != nil {
		t.Fatal(err)
	}
	uploaded, err := manager.Upload(context.Background(), created.Path)
	if err != nil {
		t.Fatalf("Upload() through rclone crypt failed: %v", err)
	}
	items, err := manager.List(context.Background())
	if err != nil || len(items) != 1 || items[0].Name != uploaded.Name {
		t.Fatalf("List() through rclone crypt = %#v, %v", items, err)
	}
	wrong, err := runner.Run(context.Background(), Request{Args: []string{"obscure", "-"}, Stdin: []byte("wrong-synthetic-recovery-password\n")})
	if err != nil {
		t.Fatal(err)
	}
	wrongManager := manager
	wrongManager.CryptPassword = strings.TrimSpace(wrong.Stdout)
	wrongItems, wrongErr := wrongManager.List(context.Background())
	if wrongErr == nil && len(wrongItems) != 0 {
		t.Fatalf("wrong recovery material exposed protected cloud names: %#v", wrongItems)
	}
	if err := os.Remove(created.Path); err != nil {
		t.Fatal(err)
	}
	downloaded, err := manager.Download(context.Background(), uploaded.Name)
	if err != nil {
		t.Fatalf("Download() through rclone crypt failed: %v", err)
	}
	if _, err := snapshot.Validate(downloaded, limits.Default()); err != nil {
		t.Fatalf("rclone crypt roundtrip produced an invalid snapshot: %v", err)
	}
}

func cloudTestManager(t *testing.T, runner Runner) Manager {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, "config", "rclone.conf")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("synthetic encrypted config"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Manager{
		Runner: runner, SnapshotDirectory: filepath.Join(root, "snapshots"), StateDirectory: filepath.Join(root, "state"), ConfigPath: configPath,
		CryptRemote: "deck-snapshot-crypt", BaseRemote: "deck-snapshot-drive", ExpectedBaseType: "local",
		ProtectionFingerprint: strings.Repeat("a", 64), CryptPassword: "synthetic-obscured-password", CryptPassword2: "synthetic-obscured-password-two",
		AllowUnencryptedTest: true, Limits: limits.Default(),
	}
}

func createCloudTestSnapshot(t *testing.T) snapshot.Created {
	t.Helper()
	data := []byte(`{"status":"synthetic"}`)
	digest := sha256.Sum256(data)
	entry := manifest.File{LogicalPath: "reports/discovery.json", Component: "reports", Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), Mode: 0o600, Generated: true}
	value := manifest.New("dsnap-cloudfixture", "phase4-test", "ds-cloud-device", "Cloud fixture", time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC))
	value.Files = []manifest.File{entry}
	value.Normalize()
	created, err := snapshot.Create(context.Background(), t.TempDir(), discovery.Result{Manifest: value, Candidates: []discovery.Candidate{{Entry: entry, Data: data}}}, limits.Default())
	if err != nil {
		t.Fatal(err)
	}
	return created
}
