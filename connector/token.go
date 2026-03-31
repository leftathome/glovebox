package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TokenSource provides bearer tokens for authenticated API requests.
// Connectors call Token() before each API request.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticTokenSource wraps a fixed token string. Suitable for PATs,
// API keys, and app passwords that do not require refresh.
type StaticTokenSource struct {
	token string
}

// NewStaticTokenSource creates a TokenSource that always returns the
// given token. The token is not validated -- if it is invalid, the
// upstream API will return an appropriate error (e.g. 401).
func NewStaticTokenSource(token string) TokenSource {
	return &StaticTokenSource{token: token}
}

// Token returns the static token. Context is accepted for interface
// compatibility but is not used -- the call is instantaneous.
func (s *StaticTokenSource) Token(ctx context.Context) (string, error) {
	return s.token, nil
}

// OAuthConfig holds the OAuth2 client credentials and token endpoint
// needed to refresh access tokens.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
	Scopes       []string
}

// tokenFile is the on-disk representation of an OAuth2 token.
type tokenFile struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	Expiry       time.Time `json:"expiry"`
}

// expiryBuffer is subtracted from the token expiry time to avoid using
// a token that is about to expire.
const expiryBuffer = 30 * time.Second

// RefreshableTokenSource manages an OAuth2 access token with automatic
// refresh and atomic file persistence. Concurrent Token() calls are
// serialized via sync.Mutex so only one goroutine refreshes at a time.
type RefreshableTokenSource struct {
	config  OAuthConfig
	path    string // stateDir/token.json
	mu      sync.Mutex
	current *tokenFile
}

// tokenResponse is the JSON structure returned by an OAuth2 token endpoint.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

// NewRefreshableTokenSource creates a TokenSource backed by an OAuth2
// refresh token stored in stateDir/token.json. If the token file does
// not exist, a PermanentError is returned instructing the operator to
// run the authentication setup flow.
func NewRefreshableTokenSource(config OAuthConfig, stateDir string) (TokenSource, error) {
	path := filepath.Join(stateDir, "token.json")

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, PermanentError(fmt.Errorf(
				"token file not found at %s; run 'glovebox-auth setup' to authenticate", path))
		}
		return nil, fmt.Errorf("read token file: %w", err)
	}

	var tf tokenFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return nil, fmt.Errorf("parse token file: %w", err)
	}

	return &RefreshableTokenSource{
		config:  config,
		path:    path,
		current: &tf,
	}, nil
}

// Token returns a valid access token, refreshing it if necessary.
// If the current token has not expired (with a 30-second buffer),
// it is returned immediately. Otherwise a refresh is performed.
func (r *RefreshableTokenSource) Token(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.current != nil && time.Now().Before(r.current.Expiry.Add(-expiryBuffer)) {
		return r.current.AccessToken, nil
	}

	return r.refresh(ctx)
}

// refresh performs the OAuth2 token refresh flow, persists the new token
// atomically, and returns the new access token.
func (r *RefreshableTokenSource) refresh(ctx context.Context) (string, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {r.current.RefreshToken},
		"client_id":     {r.config.ClientID},
		"client_secret": {r.config.ClientSecret},
	}
	if len(r.config.Scopes) > 0 {
		form.Set("scope", strings.Join(r.config.Scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.config.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("token refresh request: %w", err)
	}
	defer resp.Body.Close()

	const maxResponseBytes = 1 << 20 // 1 MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return "", PermanentError(fmt.Errorf(
			"token refresh returned 401: re-authenticate with 'glovebox-auth setup'"))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parse refresh response: %w", err)
	}

	if tr.Error == "invalid_grant" {
		return "", PermanentError(fmt.Errorf(
			"token refresh returned invalid_grant: re-authenticate with 'glovebox-auth setup'"))
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token refresh failed with status %d: %s", resp.StatusCode, string(body))
	}

	newToken := &tokenFile{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Expiry:       time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}

	if err := r.persist(newToken); err != nil {
		return "", fmt.Errorf("persist refreshed token: %w", err)
	}

	r.current = newToken
	return newToken.AccessToken, nil
}

// ClientCredentialsTokenSource manages an OAuth2 access token obtained
// via the client_credentials grant type. Tokens are cached in memory
// and re-fetched automatically when they expire. Thread-safe.
type ClientCredentialsTokenSource struct {
	config OAuthConfig
	mu     sync.Mutex
	token  string
	expiry time.Time
}

// NewClientCredentialsTokenSource creates a TokenSource that obtains
// tokens using the OAuth2 client_credentials grant. TokenURL, ClientID,
// and ClientSecret must all be non-empty.
func NewClientCredentialsTokenSource(config OAuthConfig) (TokenSource, error) {
	if config.TokenURL == "" {
		return nil, fmt.Errorf("client credentials: TokenURL must not be empty")
	}
	if config.ClientID == "" {
		return nil, fmt.Errorf("client credentials: ClientID must not be empty")
	}
	if config.ClientSecret == "" {
		return nil, fmt.Errorf("client credentials: ClientSecret must not be empty")
	}
	return &ClientCredentialsTokenSource{config: config}, nil
}

// Token returns a valid access token, fetching one from the token
// endpoint if the cached token has expired or is not yet set.
func (c *ClientCredentialsTokenSource) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.expiry.Add(-expiryBuffer)) {
		return c.token, nil
	}

	return c.authenticate(ctx)
}

// authenticate performs the OAuth2 client_credentials grant, caches the
// result, and returns the new access token.
func (c *ClientCredentialsTokenSource) authenticate(ctx context.Context) (string, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.config.ClientID},
		"client_secret": {c.config.ClientSecret},
	}
	if len(c.config.Scopes) > 0 {
		form.Set("scope", strings.Join(c.config.Scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.TokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build client credentials request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("client credentials request: %w", err)
	}
	defer resp.Body.Close()

	const maxResponseBytes = 1 << 20 // 1 MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("read client credentials response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return "", PermanentError(fmt.Errorf(
			"client credentials token request returned 401: check client_id and client_secret"))
	}

	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parse client credentials response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("client credentials token request failed with status %d: %s",
			resp.StatusCode, string(body))
	}

	if tr.AccessToken == "" {
		return "", fmt.Errorf("client credentials response contained empty access_token")
	}

	c.token = tr.AccessToken
	c.expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.token, nil
}

// persist writes the token file atomically using a temp file and rename,
// following the same pattern as checkpoint.go.
func (r *RefreshableTokenSource) persist(tf *tokenFile) error {
	data, err := json.Marshal(tf)
	if err != nil {
		return fmt.Errorf("marshal token: %w", err)
	}

	tmpPath := r.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("write token tmp: %w", err)
	}
	if err := os.Rename(tmpPath, r.path); err != nil {
		return fmt.Errorf("rename token: %w", err)
	}
	return nil
}
