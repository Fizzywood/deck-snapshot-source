package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/browseropen"
)

const (
	googleAuthorizationEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenEndpoint         = "https://oauth2.googleapis.com/token"
	googleDriveFileScope        = "https://www.googleapis.com/auth/drive.file"
	googleOAuthMaximumBody      = 1024 * 1024
	googleOAuthTimeout          = 30 * time.Minute
)

var oauthErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type googleOAuthDependencies struct {
	authorizationEndpoint string
	tokenEndpoint         string
	client                *http.Client
	openURL               func(string) error
	now                   func() time.Time
	timeout               time.Duration
}

type oauthCallback struct {
	code string
	err  error
}

type googleTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
}

type rcloneOAuthToken struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

func (manager Manager) obtainGoogleToken(ctx context.Context, clientID, clientCredential string) ([]byte, error) {
	dependencies, err := manager.googleOAuthDependencies()
	if err != nil {
		return nil, err
	}
	verifier, err := randomOAuthValue(64)
	if err != nil {
		return nil, errors.New("generate Google PKCE verifier")
	}
	state, err := randomOAuthValue(32)
	if err != nil {
		return nil, errors.New("generate Google OAuth state")
	}
	challengeBytes := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("start private Google OAuth callback listener")
	}
	defer listener.Close()
	redirectURL := "http://" + listener.Addr().String() + "/"

	result := make(chan oauthCallback, 1)
	var delivered sync.Once
	deliver := func(value oauthCallback) { delivered.Do(func() { result <- value }) }
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Cache-Control", "no-store")
		response.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		if request.Method != http.MethodGet || request.URL.Path != "/" || len(request.RequestURI) > 8192 {
			http.Error(response, "Authorization response rejected.", http.StatusBadRequest)
			return
		}
		remoteHost, _, splitErr := net.SplitHostPort(request.RemoteAddr)
		remoteIP := net.ParseIP(remoteHost)
		values := request.URL.Query()
		states, statePresent := values["state"]
		if splitErr != nil || remoteIP == nil || !remoteIP.IsLoopback() || !statePresent || len(states) != 1 ||
			subtle.ConstantTimeCompare([]byte(states[0]), []byte(state)) != 1 {
			http.Error(response, "Authorization response rejected.", http.StatusBadRequest)
			return
		}
		if oauthErrors := values["error"]; len(oauthErrors) == 1 && oauthErrors[0] != "" {
			deliver(oauthCallback{err: errors.New("Google authorization was declined or failed")})
			writeOAuthCompletion(response, false)
			return
		}
		codes := values["code"]
		if len(codes) != 1 || !validOAuthSecret(codes[0], 8192) {
			http.Error(response, "Authorization response rejected.", http.StatusBadRequest)
			return
		}
		deliver(oauthCallback{code: codes[0]})
		writeOAuthCompletion(response, true)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 16 * 1024}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(listener)
		close(serveDone)
	}()
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
		<-serveDone
	}()

	authorizationURL, err := buildGoogleAuthorizationURL(dependencies.authorizationEndpoint, clientID, redirectURL, state, challenge)
	if err != nil {
		return nil, err
	}
	if err := dependencies.openURL(authorizationURL); err != nil {
		return nil, fmt.Errorf("open system browser for Google authorization: %w", err)
	}
	waitContext, cancel := context.WithTimeout(ctx, dependencies.timeout)
	defer cancel()
	var callback oauthCallback
	select {
	case <-waitContext.Done():
		return nil, errors.New("Google authorization did not complete before its deadline")
	case callback = <-result:
	}
	if callback.err != nil {
		return nil, callback.err
	}
	return exchangeGoogleAuthorizationCode(waitContext, dependencies, clientID, clientCredential, redirectURL, verifier, callback.code)
}

func (manager Manager) googleOAuthDependencies() (googleOAuthDependencies, error) {
	dependencies := manager.googleOAuth
	if dependencies.authorizationEndpoint == "" {
		dependencies.authorizationEndpoint = googleAuthorizationEndpoint
	}
	if dependencies.tokenEndpoint == "" {
		dependencies.tokenEndpoint = googleTokenEndpoint
	}
	if dependencies.openURL == nil {
		dependencies.openURL = browseropen.OpenAuthorizationURL
	}
	if dependencies.now == nil {
		dependencies.now = time.Now
	}
	if dependencies.timeout == 0 {
		dependencies.timeout = googleOAuthTimeout
	}
	if dependencies.timeout <= 0 || dependencies.timeout > googleOAuthTimeout {
		return googleOAuthDependencies{}, errors.New("Google OAuth timeout is outside the allowed range")
	}
	if dependencies.client == nil {
		dependencies.client = &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if !manager.AllowUnencryptedTest && (dependencies.authorizationEndpoint != googleAuthorizationEndpoint || dependencies.tokenEndpoint != googleTokenEndpoint) {
		return googleOAuthDependencies{}, errors.New("Google OAuth endpoints are not the required production endpoints")
	}
	return dependencies, nil
}

func buildGoogleAuthorizationURL(endpoint, clientID, redirectURL, state, challenge string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("Google authorization endpoint is invalid")
	}
	query := parsed.Query()
	query.Set("client_id", clientID)
	query.Set("redirect_uri", redirectURL)
	query.Set("response_type", "code")
	query.Set("scope", googleDriveFileScope)
	query.Set("access_type", "offline")
	query.Set("prompt", "consent")
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func exchangeGoogleAuthorizationCode(ctx context.Context, dependencies googleOAuthDependencies, clientID, clientCredential, redirectURL, verifier, code string) ([]byte, error) {
	values := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientCredential},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURL},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, dependencies.tokenEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, errors.New("prepare Google token exchange")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := dependencies.client.Do(request)
	if err != nil {
		return nil, errors.New("Google token exchange request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, googleOAuthMaximumBody+1))
	if err != nil || len(body) > googleOAuthMaximumBody {
		return nil, errors.New("Google token exchange response exceeded its safe bound")
	}
	var token googleTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, errors.New("Google token exchange returned an invalid response")
	}
	if response.StatusCode != http.StatusOK {
		if oauthErrorCodePattern.MatchString(token.Error) {
			return nil, fmt.Errorf("Google token exchange failed with error code %s", token.Error)
		}
		return nil, fmt.Errorf("Google token exchange failed with HTTP status %d", response.StatusCode)
	}
	if !validOAuthSecret(token.AccessToken, 16384) || !validOAuthSecret(token.RefreshToken, 16384) ||
		!strings.EqualFold(token.TokenType, "Bearer") || token.ExpiresIn <= 0 || token.ExpiresIn > int64((24*time.Hour)/time.Second) {
		return nil, errors.New("Google token exchange returned incomplete credentials")
	}
	if strings.TrimSpace(token.Scope) != googleDriveFileScope {
		return nil, errors.New("Google granted an unexpected OAuth scope")
	}
	expiresAt := dependencies.now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second)
	encoded, err := json.Marshal(rcloneOAuthToken{
		AccessToken: token.AccessToken, TokenType: "Bearer", RefreshToken: token.RefreshToken, Expiry: expiresAt,
	})
	if err != nil {
		return nil, errors.New("encode protected Google token")
	}
	return encoded, nil
}

func randomOAuthValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func validOAuthSecret(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func validGoogleDesktopCredential(value string) bool {
	return len(value) >= 8 && validOAuthSecret(value, 512)
}

func writeOAuthCompletion(response http.ResponseWriter, success bool) {
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	status := "Authorization failed"
	message := "Return to Deck Snapshot and try again."
	if success {
		status = "Authorization complete"
		message = "You can close this tab and return to Deck Snapshot."
	}
	_, _ = io.WriteString(response, "<!doctype html><meta charset=utf-8><title>Deck Snapshot</title><main><h1>"+status+"</h1><p>"+message+"</p></main>")
}
