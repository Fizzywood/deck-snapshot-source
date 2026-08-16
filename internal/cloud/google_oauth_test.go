package cloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGooglePKCERejectsMismatchedCallbackState(t *testing.T) {
	manager := Manager{AllowUnencryptedTest: true}
	manager.googleOAuth = googleOAuthDependencies{
		authorizationEndpoint: googleAuthorizationEndpoint,
		tokenEndpoint:         "http://127.0.0.1:1/token",
		client:                &http.Client{Timeout: time.Second},
		timeout:               100 * time.Millisecond,
		now:                   time.Now,
		openURL: func(value string) error {
			parsed, err := url.Parse(value)
			if err != nil {
				return err
			}
			callback, err := url.Parse(parsed.Query().Get("redirect_uri"))
			if err != nil {
				return err
			}
			query := callback.Query()
			query.Set("state", "mismatched-state")
			query.Set("code", "must-not-be-accepted")
			callback.RawQuery = query.Encode()
			response, err := http.Get(callback.String())
			if err != nil {
				return err
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			if response.StatusCode != http.StatusBadRequest {
				return errors.New("mismatched state was not rejected")
			}
			return nil
		},
	}
	if _, err := manager.obtainGoogleToken(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential); err == nil || !strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "mismatched-state") {
		t.Fatalf("mismatched OAuth state error = %v", err)
	}
}

func TestGooglePKCEFailureReturnsOnlyBoundedErrorCode(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(response, `{"error":"invalid_grant","error_description":"sensitive-provider-detail"}`)
	}))
	defer tokenServer.Close()
	manager := Manager{AllowUnencryptedTest: true}
	manager.googleOAuth = googleOAuthDependencies{
		authorizationEndpoint: googleAuthorizationEndpoint,
		tokenEndpoint:         tokenServer.URL,
		client:                tokenServer.Client(),
		timeout:               time.Second,
		now:                   time.Now,
		openURL:               successfulSyntheticCallback,
	}
	_, err := manager.obtainGoogleToken(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential)
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "sensitive-provider-detail") || strings.Contains(err.Error(), "synthetic-code") {
		t.Fatalf("unsafe Google token error = %v", err)
	}
}

func TestGooglePKCERejectsUnexpectedGrantedScope(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","scope":"https://www.googleapis.com/auth/drive","expires_in":3600}`)
	}))
	defer tokenServer.Close()
	manager := Manager{AllowUnencryptedTest: true}
	manager.googleOAuth = googleOAuthDependencies{
		authorizationEndpoint: googleAuthorizationEndpoint,
		tokenEndpoint:         tokenServer.URL,
		client:                tokenServer.Client(),
		timeout:               time.Second,
		now:                   time.Now,
		openURL:               successfulSyntheticCallback,
	}
	if _, err := manager.obtainGoogleToken(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential); err == nil || !strings.Contains(err.Error(), "unexpected OAuth scope") {
		t.Fatalf("unexpected scope error = %v", err)
	}
}

func TestGooglePKCEAcceptsScopeSetInEitherOrder(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","scope":"https://www.googleapis.com/auth/drive.appdata https://www.googleapis.com/auth/drive.file","expires_in":3600}`)
	}))
	defer tokenServer.Close()
	manager := Manager{AllowUnencryptedTest: true}
	manager.googleOAuth = googleOAuthDependencies{
		authorizationEndpoint: googleAuthorizationEndpoint,
		tokenEndpoint:         tokenServer.URL,
		client:                tokenServer.Client(),
		timeout:               time.Second,
		now:                   time.Now,
		openURL:               successfulSyntheticCallback,
	}
	if _, err := manager.obtainGoogleToken(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential); err != nil {
		t.Fatalf("scope set was rejected: %v", err)
	}
}

func TestGooglePKCERejectsPartialOrDuplicateScopeSet(t *testing.T) {
	for name, scope := range map[string]string{
		"partial":   googleDriveFileScope,
		"duplicate": googleDriveFileScope + " " + googleDriveFileScope,
		"extra":     googleDriveFileScope + " https://www.googleapis.com/auth/drive",
		"newline":   googleDriveFileScope + "\n" + googleDriveAppDataScope,
	} {
		t.Run(name, func(t *testing.T) {
			tokenServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(response, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","token_type":"Bearer","scope":"`+scope+`","expires_in":3600}`)
			}))
			defer tokenServer.Close()
			manager := Manager{AllowUnencryptedTest: true}
			manager.googleOAuth = googleOAuthDependencies{
				authorizationEndpoint: googleAuthorizationEndpoint,
				tokenEndpoint:         tokenServer.URL,
				client:                tokenServer.Client(),
				timeout:               time.Second,
				now:                   time.Now,
				openURL:               successfulSyntheticCallback,
			}
			if _, err := manager.obtainGoogleToken(context.Background(), "synthetic.apps.googleusercontent.com", testGoogleDesktopCredential); err == nil {
				t.Fatal("malformed scope set was accepted")
			}
		})
	}
}

func successfulSyntheticCallback(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	callback, err := url.Parse(parsed.Query().Get("redirect_uri"))
	if err != nil {
		return err
	}
	query := callback.Query()
	query.Set("state", parsed.Query().Get("state"))
	query.Set("code", "synthetic-code")
	callback.RawQuery = query.Encode()
	response, err := http.Get(callback.String())
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		return errors.New("synthetic callback failed")
	}
	return nil
}
