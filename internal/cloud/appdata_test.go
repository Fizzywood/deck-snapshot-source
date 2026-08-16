package cloud

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type appDataTestFile struct {
	id      string
	name    string
	mime    string
	size    int
	content []byte
}

func TestLookupRecoveryObjectAcceptsIdenticalDuplicates(t *testing.T) {
	material, err := GenerateRecovery(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	contents, err := encodeRecovery(material)
	if err != nil {
		t.Fatal(err)
	}
	manager, cleanup := newAppDataTestManager(t, []appDataTestFile{
		{id: "one", name: managedRecoveryObjectName, mime: "application/json", size: len(contents), content: contents},
		{id: "two", name: managedRecoveryObjectName, mime: "application/json", size: len(contents), content: contents},
	})
	defer cleanup()
	lookup, err := manager.lookupRecoveryObject(context.Background(), "synthetic-access")
	if err != nil || lookup.State != appDataValid || lookup.ExistingCount != 2 || lookup.Fingerprint != mustRecoveryFingerprint(material) || !strings.Contains(lookup.Warning, "identical") {
		t.Fatalf("identical duplicate lookup = %#v, %v", lookup, err)
	}
}

func TestLookupRecoveryObjectRejectsConflictingOrInvalidObjects(t *testing.T) {
	first, err := GenerateRecovery(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateRecovery(time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, err := encodeRecovery(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := encodeRecovery(second)
	if err != nil {
		t.Fatal(err)
	}
	manager, cleanup := newAppDataTestManager(t, []appDataTestFile{
		{id: "one", name: managedRecoveryObjectName, mime: "application/json", size: len(firstBytes), content: firstBytes},
		{id: "two", name: managedRecoveryObjectName, mime: "application/json", size: len(secondBytes), content: secondBytes},
	})
	defer cleanup()
	lookup, err := manager.lookupRecoveryObject(context.Background(), "synthetic-access")
	if err != nil || lookup.State != appDataConflict {
		t.Fatalf("conflicting lookup = %#v, %v", lookup, err)
	}

	manager, cleanup = newAppDataTestManager(t, []appDataTestFile{{id: "bad", name: managedRecoveryObjectName, mime: "application/json", size: 2, content: []byte(`{}`)}})
	defer cleanup()
	lookup, err = manager.lookupRecoveryObject(context.Background(), "synthetic-access")
	if err != nil || lookup.State != appDataInvalid {
		t.Fatalf("invalid lookup = %#v, %v", lookup, err)
	}
}

func TestLookupRecoveryObjectRejectsOversizedMetadataAndMissingObject(t *testing.T) {
	manager, cleanup := newAppDataTestManager(t, []appDataTestFile{{id: "large", name: managedRecoveryObjectName, mime: "application/json", size: maximumRecoverySize + 1}})
	defer cleanup()
	lookup, err := manager.lookupRecoveryObject(context.Background(), "synthetic-access")
	if err != nil || lookup.State != appDataInvalid {
		t.Fatalf("oversized lookup = %#v, %v", lookup, err)
	}

	manager, cleanup = newAppDataTestManager(t, nil)
	defer cleanup()
	lookup, err = manager.lookupRecoveryObject(context.Background(), "synthetic-access")
	if err != nil || lookup.State != appDataMissing {
		t.Fatalf("missing lookup = %#v, %v", lookup, err)
	}
}

func TestDriveAPIEndpointAndResponseBounds(t *testing.T) {
	var strictListing driveFileList
	if err := decodeStrictDriveJSON([]byte(`{"files":[]} {"files":[]}`), &strictListing); err == nil {
		t.Fatal("trailing Drive JSON was accepted")
	}
	manager := Manager{googleOAuth: googleOAuthDependencies{driveAPIEndpoint: "https://attacker.example/drive/v3/files"}}
	if _, err := manager.driveAPIURL("?q=safe"); err == nil {
		t.Fatal("unapproved Google Drive API host was accepted")
	}
	manager.googleOAuth.driveAPIEndpoint = "https://www.googleapis.com/drive/v3/files"
	if _, err := manager.driveAPIURL("https://attacker.example/exfiltrate"); err == nil {
		t.Fatal("absolute Drive API suffix was accepted")
	}
	manager.googleOAuth.driveUploadEndpoint = "https://attacker.example/upload/drive/v3/files"
	if _, err := manager.driveUploadURL("?uploadType=multipart"); err == nil {
		t.Fatal("unapproved Google Drive upload host was accepted")
	}

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(make([]byte, googleDriveMaximumBody+1))
	}))
	defer server.Close()
	manager, cleanup := newAppDataTestManager(t, nil)
	defer cleanup()
	manager.googleOAuth.driveAPIEndpoint = server.URL + "/drive/v3/files"
	if _, err := manager.listDriveFiles(context.Background(), "synthetic-access", "appDataFolder", "true", 1); err == nil || !strings.Contains(err.Error(), "safe bound") {
		t.Fatalf("oversized Drive response was accepted: %v", err)
	}

	paginationServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"files":[],"nextPageToken":"bounded-token"}`)
	}))
	defer paginationServer.Close()
	manager.googleOAuth.driveAPIEndpoint = paginationServer.URL + "/drive/v3/files"
	if _, err := manager.listDriveFiles(context.Background(), "synthetic-access", "appDataFolder", "true", 1); err == nil || !strings.Contains(err.Error(), "page bound") {
		t.Fatalf("unbounded Drive pagination was accepted: %v", err)
	}

	redirectServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Location", "https://attacker.example/recovery")
		response.WriteHeader(http.StatusFound)
	}))
	defer redirectServer.Close()
	manager, cleanup = newAppDataTestManager(t, nil)
	defer cleanup()
	manager.googleOAuth.driveAPIEndpoint = redirectServer.URL + "/drive/v3/files"
	if _, err := manager.listDriveFiles(context.Background(), "synthetic-access", "appDataFolder", "true", 1); err == nil || !strings.Contains(err.Error(), "HTTP status 302") {
		t.Fatalf("redirecting Drive response was accepted: %v", err)
	}

	timeoutServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(response, `{"files":[]}`)
	}))
	defer timeoutServer.Close()
	manager, cleanup = newAppDataTestManager(t, nil)
	defer cleanup()
	manager.googleOAuth.driveAPIEndpoint = timeoutServer.URL + "/drive/v3/files"
	manager.googleOAuth.client = &http.Client{Timeout: 5 * time.Millisecond}
	if _, err := manager.listDriveFiles(context.Background(), "synthetic-access", "appDataFolder", "true", 1); err == nil || !strings.Contains(err.Error(), "request failed") {
		t.Fatalf("unbounded Drive request was accepted: %v", err)
	}
}

func TestCreateRecoveryObjectReadsBackWithoutReplacement(t *testing.T) {
	material, err := GenerateRecovery(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var stored []byte
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		methods = append(methods, request.Method)
		response.Header().Set("Content-Type", "application/json")
		if request.Method == http.MethodPost {
			if request.URL.Path != "/upload/drive/v3/files" || request.URL.Query().Get("uploadType") != "multipart" {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			mediaType, parameters, parseErr := mime.ParseMediaType(request.Header.Get("Content-Type"))
			if parseErr != nil || mediaType != "multipart/related" {
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			reader := multipart.NewReader(request.Body, parameters["boundary"])
			for {
				part, partErr := reader.NextPart()
				if partErr == io.EOF {
					break
				}
				if partErr != nil {
					response.WriteHeader(http.StatusBadRequest)
					return
				}
				value, readErr := io.ReadAll(part)
				_ = part.Close()
				if readErr != nil {
					response.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(value) > 0 && value[0] == '{' && strings.Contains(string(value), `"crypt_password"`) {
					stored = value
				}
			}
			_, _ = io.WriteString(response, `{"id":"created","name":"deck-snapshot-recovery-v1.json","mimeType":"application/json"}`)
			return
		}
		if request.Method != http.MethodGet {
			response.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if request.URL.Query().Get("alt") == "media" {
			_, _ = response.Write(stored)
			return
		}
		if len(stored) == 0 {
			_, _ = io.WriteString(response, `{"files":[]}`)
			return
		}
		_, _ = io.WriteString(response, `{"files":[{"id":"created","name":"deck-snapshot-recovery-v1.json","mimeType":"application/json","size":"`+strconv.Itoa(len(stored))+`"}]}`)
	}))
	defer server.Close()
	manager := Manager{AllowUnencryptedTest: true}
	manager.googleOAuth = googleOAuthDependencies{driveAPIEndpoint: server.URL + "/drive/v3/files", driveUploadEndpoint: server.URL + "/upload/drive/v3/files", client: server.Client(), timeout: time.Second, now: time.Now}
	lookup, err := manager.createRecoveryObject(context.Background(), "synthetic-access", material)
	if err != nil || lookup.State != appDataValid || lookup.Fingerprint != mustRecoveryFingerprint(material) {
		t.Fatalf("create/read-back = %#v, %v", lookup, err)
	}
	for _, method := range methods {
		if method == http.MethodPatch || method == http.MethodDelete {
			t.Fatalf("recovery object used destructive method %s", method)
		}
	}
	if len(stored) == 0 {
		t.Fatal("recovery content was not sent to the create endpoint")
	}
}

func newAppDataTestManager(t *testing.T, files []appDataTestFile) (Manager, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			response.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if request.URL.Query().Get("alt") == "media" {
			for _, file := range files {
				if request.URL.Path == "/drive/v3/files/"+file.id {
					response.Header().Set("Content-Type", "application/json")
					if file.content == nil {
						response.WriteHeader(http.StatusNotFound)
						return
					}
					_, _ = response.Write(file.content)
					return
				}
			}
			response.WriteHeader(http.StatusNotFound)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("spaces") != "appDataFolder" {
			_, _ = io.WriteString(response, `{"files":[]}`)
			return
		}
		value := struct {
			Files []map[string]any `json:"files"`
		}{Files: make([]map[string]any, 0, len(files))}
		for _, file := range files {
			value.Files = append(value.Files, map[string]any{"id": file.id, "name": file.name, "mimeType": file.mime, "size": stringSize(file.size)})
		}
		if err := json.NewEncoder(response).Encode(value); err != nil {
			t.Errorf("encode synthetic Drive listing: %v", err)
		}
	}))
	manager := Manager{AllowUnencryptedTest: true}
	manager.googleOAuth = googleOAuthDependencies{
		driveAPIEndpoint:    server.URL + "/drive/v3/files",
		driveUploadEndpoint: server.URL + "/upload/drive/v3/files",
		client:              server.Client(),
		timeout:             time.Second,
		now:                 time.Now,
	}
	return manager, server.Close
}

func stringSize(value int) string {
	return strconv.Itoa(value)
}
