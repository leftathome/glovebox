package connector

import "context"

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
