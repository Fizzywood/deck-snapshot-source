package cloud

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	managedRecoveryObjectName = "deck-snapshot-recovery-v1.json"
	googleDriveMaximumBody    = 256 * 1024
	googleDriveMaximumPages   = 8
	googleDrivePageSize       = 100
	googleFolderMimeType      = "application/vnd.google-apps.folder"
)

var googleDriveFileIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)

type driveFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size,string"`
}

type driveFileList struct {
	Files         []driveFile `json:"files"`
	NextPageToken string      `json:"nextPageToken"`
}

type appDataState int

const (
	appDataMissing appDataState = iota
	appDataValid
	appDataInvalid
	appDataConflict
)

type appDataLookup struct {
	State         appDataState
	Material      RecoveryMaterial
	Fingerprint   string
	Warning       string
	ExistingCount int
}

func (manager Manager) lookupRecoveryObject(ctx context.Context, accessToken string) (appDataLookup, error) {
	files, err := manager.listDriveFiles(ctx, accessToken, "appDataFolder", "name = '"+managedRecoveryObjectName+"' and trashed = false", googleDrivePageSize)
	if err != nil {
		return appDataLookup{}, fmt.Errorf("list Deck Snapshot recovery data: %w", err)
	}
	if len(files) == 0 {
		return appDataLookup{State: appDataMissing}, nil
	}
	lookup := appDataLookup{State: appDataValid, ExistingCount: len(files)}
	var firstFingerprint string
	for _, file := range files {
		if file.Name != managedRecoveryObjectName || file.MimeType != "application/json" || file.ID == "" || file.Size <= 0 || file.Size > maximumRecoverySize {
			lookup.State = appDataInvalid
			continue
		}
		contents, err := manager.downloadDriveFile(ctx, accessToken, file.ID)
		if err != nil {
			lookup.State = appDataInvalid
			continue
		}
		material, err := ParseRecovery(contents)
		if err != nil {
			lookup.State = appDataInvalid
			continue
		}
		fingerprint, err := RecoveryFingerprint(material)
		if err != nil {
			lookup.State = appDataInvalid
			continue
		}
		if firstFingerprint == "" {
			firstFingerprint = fingerprint
			lookup.Material = material
			lookup.Fingerprint = fingerprint
			continue
		}
		if fingerprint != firstFingerprint {
			lookup.State = appDataConflict
		}
	}
	if lookup.State == appDataValid && lookup.ExistingCount > 1 {
		lookup.Warning = "multiple identical Google Drive recovery objects were found; the matching copies were retained"
	}
	if lookup.State == appDataValid && firstFingerprint == "" {
		lookup.State = appDataInvalid
	}
	return lookup, nil
}

func (manager Manager) createRecoveryObject(ctx context.Context, accessToken string, material RecoveryMaterial) (appDataLookup, error) {
	encoded, err := encodeRecovery(material)
	if err != nil {
		return appDataLookup{}, err
	}
	defer clear(encoded)
	metadata, err := json.Marshal(struct {
		Name     string   `json:"name"`
		Parents  []string `json:"parents"`
		MimeType string   `json:"mimeType"`
	}{Name: managedRecoveryObjectName, Parents: []string{"appDataFolder"}, MimeType: "application/json"})
	if err != nil {
		return appDataLookup{}, errors.New("encode Google Drive recovery metadata")
	}
	boundaryBytes := make([]byte, 18)
	if _, err := rand.Read(boundaryBytes); err != nil {
		return appDataLookup{}, errors.New("generate Google Drive recovery request boundary")
	}
	boundary := "DeckSnapshot" + fmt.Sprintf("%x", boundaryBytes)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.SetBoundary(boundary); err != nil {
		return appDataLookup{}, errors.New("prepare Google Drive recovery request")
	}
	metadataHeader := textproto.MIMEHeader{}
	metadataHeader.Set("Content-Type", "application/json; charset=UTF-8")
	part, err := writer.CreatePart(metadataHeader)
	if err != nil {
		return appDataLookup{}, errors.New("prepare Google Drive recovery metadata")
	}
	if _, err := part.Write(metadata); err != nil {
		return appDataLookup{}, errors.New("encode Google Drive recovery metadata")
	}
	mediaHeader := textproto.MIMEHeader{}
	mediaHeader.Set("Content-Type", "application/json")
	part, err = writer.CreatePart(mediaHeader)
	if err != nil {
		return appDataLookup{}, errors.New("prepare Google Drive recovery content")
	}
	if _, err := part.Write(encoded); err != nil {
		return appDataLookup{}, errors.New("encode Google Drive recovery content")
	}
	if err := writer.Close(); err != nil {
		return appDataLookup{}, errors.New("finish Google Drive recovery request")
	}
	if body.Len() > googleDriveMaximumBody {
		return appDataLookup{}, errors.New("Google Drive recovery request exceeded its safe bound")
	}
	endpoint, err := manager.driveUploadURL("?uploadType=multipart&fields=id,name,mimeType")
	if err != nil {
		return appDataLookup{}, err
	}
	payload := body.Bytes()
	responseBody, requestErr := manager.driveRequest(ctx, http.MethodPost, endpoint, accessToken, bytes.NewReader(payload), "multipart/related; boundary="+boundary)
	clear(payload)
	if requestErr != nil {
		return appDataLookup{}, fmt.Errorf("create Google Drive recovery data: %w", requestErr)
	}
	var created driveFile
	if err := decodeStrictDriveJSON(responseBody, &created); err != nil || !googleDriveFileIDPattern.MatchString(created.ID) || created.Name != managedRecoveryObjectName || created.MimeType != "application/json" {
		return appDataLookup{}, errors.New("Google Drive returned invalid recovery creation metadata")
	}
	lookup, err := manager.lookupRecoveryObject(ctx, accessToken)
	if err != nil {
		return appDataLookup{}, err
	}
	if lookup.State != appDataValid || lookup.Fingerprint != mustRecoveryFingerprint(material) {
		return appDataLookup{}, errors.New("Google Drive recovery data did not verify after creation")
	}
	return lookup, nil
}

func (manager Manager) hasVisibleSnapshotObjects(ctx context.Context, accessToken string) (bool, error) {
	rootFolders, err := manager.listDriveFiles(ctx, accessToken, "drive", "'root' in parents and name = 'Deck Snapshot' and mimeType = '"+googleFolderMimeType+"' and trashed = false", googleDrivePageSize)
	if err != nil {
		return false, fmt.Errorf("inspect the visible Deck Snapshot folder: %w", err)
	}
	for _, root := range rootFolders {
		if root.MimeType != googleFolderMimeType {
			continue
		}
		if !googleDriveFileIDPattern.MatchString(root.ID) {
			return false, errors.New("Google Drive returned an invalid Deck Snapshot folder ID")
		}
		snapshotFolders, err := manager.listDriveFiles(ctx, accessToken, "drive", "'"+root.ID+"' in parents and name = 'Snapshots' and mimeType = '"+googleFolderMimeType+"' and trashed = false", googleDrivePageSize)
		if err != nil {
			return false, fmt.Errorf("inspect the visible Deck Snapshot snapshot folder: %w", err)
		}
		for _, snapshotFolder := range snapshotFolders {
			if snapshotFolder.MimeType != googleFolderMimeType {
				continue
			}
			if !googleDriveFileIDPattern.MatchString(snapshotFolder.ID) {
				return false, errors.New("Google Drive returned an invalid Deck Snapshot snapshot-folder ID")
			}
			objects, err := manager.listDriveFiles(ctx, accessToken, "drive", "'"+snapshotFolder.ID+"' in parents and trashed = false", 1)
			if err != nil {
				return false, fmt.Errorf("inspect visible Deck Snapshot backups: %w", err)
			}
			if len(objects) > 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

func (manager Manager) listDriveFiles(ctx context.Context, accessToken, space, query string, pageSize int) ([]driveFile, error) {
	if pageSize <= 0 || pageSize > googleDrivePageSize {
		return nil, errors.New("Google Drive page size is outside the allowed range")
	}
	var files []driveFile
	pageToken := ""
	for page := 0; page < googleDriveMaximumPages; page++ {
		values := url.Values{}
		values.Set("spaces", space)
		values.Set("q", query)
		values.Set("pageSize", strconv.Itoa(pageSize))
		values.Set("fields", "nextPageToken,files(id,name,mimeType,size)")
		if pageToken != "" {
			if !validDrivePageToken(pageToken) {
				return nil, errors.New("Google Drive returned an invalid page token")
			}
			values.Set("pageToken", pageToken)
		}
		endpoint, err := manager.driveAPIURL("?" + values.Encode())
		if err != nil {
			return nil, err
		}
		body, err := manager.driveRequest(ctx, http.MethodGet, endpoint, accessToken, nil, "")
		if err != nil {
			return nil, fmt.Errorf("Google Drive listing failed: %w", err)
		}
		var response driveFileList
		if err := decodeStrictDriveJSON(body, &response); err != nil {
			return nil, errors.New("Google Drive returned an invalid listing")
		}
		if response.Files == nil {
			response.Files = []driveFile{}
		}
		if len(response.Files) > pageSize {
			return nil, errors.New("Google Drive returned more files than requested")
		}
		files = append(files, response.Files...)
		if response.NextPageToken == "" {
			return files, nil
		}
		pageToken = response.NextPageToken
	}
	return nil, errors.New("Google Drive listing exceeded its page bound")
}

func (manager Manager) downloadDriveFile(ctx context.Context, accessToken, id string) ([]byte, error) {
	if !googleDriveFileIDPattern.MatchString(id) {
		return nil, errors.New("Google Drive returned an invalid recovery object ID")
	}
	endpoint, err := manager.driveAPIURL("/" + id + "?alt=media")
	if err != nil {
		return nil, err
	}
	return manager.driveRequest(ctx, http.MethodGet, endpoint, accessToken, nil, "")
}

func (manager Manager) driveRequest(ctx context.Context, method, endpoint, accessToken string, body io.Reader, contentType string) ([]byte, error) {
	if !validOAuthSecret(accessToken, 16384) {
		return nil, errors.New("Google access token is invalid")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestContext, cancel := context.WithTimeout(ctx, googleDriveRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, method, endpoint, body)
	if err != nil {
		return nil, errors.New("prepare Google Drive request")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	dependencies, err := manager.googleOAuthDependencies()
	if err != nil {
		return nil, err
	}
	response, err := dependencies.client.Do(request)
	if err != nil {
		return nil, errors.New("Google Drive request failed")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, googleDriveMaximumBody+1))
	if err != nil || len(responseBody) > googleDriveMaximumBody {
		return nil, errors.New("Google Drive response exceeded its safe bound")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("Google Drive returned HTTP status %d", response.StatusCode)
	}
	return responseBody, nil
}

func (manager Manager) driveAPIURL(suffix string) (string, error) {
	dependencies, err := manager.googleOAuthDependencies()
	if err != nil {
		return "", err
	}
	base, err := url.Parse(dependencies.driveAPIEndpoint)
	if err != nil || base.Scheme != "https" || base.Host != "www.googleapis.com" || base.User != nil || base.Path != "/drive/v3/files" || base.RawQuery != "" || base.Fragment != "" {
		if !manager.AllowUnencryptedTest {
			return "", errors.New("Google Drive API endpoint is not the required production endpoint")
		}
		base, err = url.Parse(dependencies.driveAPIEndpoint)
		if err != nil || base.Scheme == "" || base.Host == "" || base.Path == "" {
			return "", errors.New("Google Drive API endpoint is invalid")
		}
	}
	if strings.HasPrefix(suffix, "/") {
		parsedSuffix, parseErr := url.Parse(suffix)
		if parseErr != nil || parsedSuffix.Host != "" || parsedSuffix.Scheme != "" || !strings.HasPrefix(parsedSuffix.Path, "/") || parsedSuffix.Fragment != "" {
			return "", errors.New("Google Drive API path is invalid")
		}
		base.Path += parsedSuffix.Path
		base.RawQuery = parsedSuffix.RawQuery
	} else if suffix == "" || strings.HasPrefix(suffix, "?") {
		base.RawQuery = strings.TrimPrefix(suffix, "?")
	} else {
		return "", errors.New("Google Drive API suffix is invalid")
	}
	return base.String(), nil
}

func (manager Manager) driveUploadURL(suffix string) (string, error) {
	dependencies, err := manager.googleOAuthDependencies()
	if err != nil {
		return "", err
	}
	base, err := url.Parse(dependencies.driveUploadEndpoint)
	if err != nil || base.Scheme != "https" || base.Host != "www.googleapis.com" || base.User != nil || base.Path != "/upload/drive/v3/files" || base.RawQuery != "" || base.Fragment != "" {
		if !manager.AllowUnencryptedTest {
			return "", errors.New("Google Drive upload endpoint is not the required production endpoint")
		}
		base, err = url.Parse(dependencies.driveUploadEndpoint)
		if err != nil || base.Scheme == "" || base.Host == "" || base.Path == "" {
			return "", errors.New("Google Drive upload endpoint is invalid")
		}
	}
	if suffix == "" || !strings.HasPrefix(suffix, "?") {
		return "", errors.New("Google Drive upload suffix is invalid")
	}
	base.RawQuery = strings.TrimPrefix(suffix, "?")
	return base.String(), nil
}

func validDrivePageToken(value string) bool {
	return validOAuthSecret(value, 4096)
}

func decodeStrictDriveJSON(body []byte, target any) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return errors.New("Google Drive response is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("Google Drive response has trailing data")
	}
	return nil
}

func mustRecoveryFingerprint(material RecoveryMaterial) string {
	fingerprint, _ := RecoveryFingerprint(material)
	return fingerprint
}
