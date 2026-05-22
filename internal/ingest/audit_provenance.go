package ingest

import "context"

// ctxKey is a private type used to namespace request-context values
// for this package, avoiding accidental collisions with other packages'
// context keys.
type ctxKey int

const deliveredByKey ctxKey = 1

// WithDeliveredBy returns a derived context carrying the validated
// source-id from the bearer token. Set by the auth middleware after
// successful validation per spec 10 §6.1; consumed downstream by the
// handler, staging writer, and audit log.
func WithDeliveredBy(ctx context.Context, sourceID string) context.Context {
	return context.WithValue(ctx, deliveredByKey, sourceID)
}

// DeliveredBy returns the source-id set by WithDeliveredBy, or
// ("", false) if absent. An empty-string value is treated as absent
// so callers can rely on the ok boolean alone.
func DeliveredBy(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(deliveredByKey).(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// Identity matches the spec 06 §5.2 shape that lands in metadata.json
// + the audit log. Defined here rather than importing the existing
// scanner-side type to avoid a circular dep.
type Identity struct {
	Provider   string `json:"provider"`
	AuthMethod string `json:"auth_method"`
	AccountID  string `json:"account_id"`
}

// BuildIdentity constructs the Identity block from a request context
// that has been through the auth middleware. Returns nil if the context
// has no delivered_by (caller writes a default or skips). Per spec 10
// §6.2, the ingest path's identity is fixed as
// {provider: "ingest", auth_method: "bearer_token", account_id: <source-id>}.
func BuildIdentity(ctx context.Context) *Identity {
	sid, ok := DeliveredBy(ctx)
	if !ok {
		return nil
	}
	return &Identity{
		Provider:   "ingest",
		AuthMethod: "bearer_token",
		AccountID:  sid,
	}
}
