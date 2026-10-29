package loginbroker

import (
	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/satoken"
)

// The broker authenticates its callers with the shared ServiceAccount-token
// check in internal/satoken (the record journal uses the same one). These
// names keep the broker's API as it was.
type (
	Caller        = satoken.Caller
	Authenticator = satoken.Authenticator
	TokenReviewer = satoken.TokenReviewer
)

var (
	// ErrUnauthenticated: see satoken.ErrUnauthenticated.
	ErrUnauthenticated = satoken.ErrUnauthenticated
	// ErrForbidden: see satoken.ErrForbidden.
	ErrForbidden = satoken.ErrForbidden
	// ErrAuthUnavailable: see satoken.ErrAuthUnavailable.
	ErrAuthUnavailable = satoken.ErrAuthUnavailable
)

// NewTokenReviewer builds the reviewer: see satoken.NewTokenReviewer.
func NewTokenReviewer(access *kubecm.Access, audience string, allowed []string) (*TokenReviewer, error) {
	return satoken.NewTokenReviewer(access, audience, allowed)
}
