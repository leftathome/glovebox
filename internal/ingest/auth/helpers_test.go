package auth

import (
	"context"
	"fmt"
	"testing"
)

// mustNewTokenStore returns a TokenStore preloaded with the given
// source-id/hex-token pairs by calling swap directly. Test-only.
func mustNewTokenStore(t *testing.T, pairs map[string]string) *TokenStore {
	t.Helper()
	s := NewTokenStore()
	entries := make([]tokenEntry, 0, len(pairs))
	for sid, hexTok := range pairs {
		tok := mustDecodeTokenForTest(t, hexTok)
		entries = append(entries, tokenEntry{token: tok, sourceID: sid})
	}
	s.swap(entries)
	return s
}

// mustDecodeTokenForTest decodes a 64-char lowercase hex string into a
// 32-byte array, failing the test on any format violation.
func mustDecodeTokenForTest(t *testing.T, s string) [32]byte {
	t.Helper()
	tok, ok := decodeToken(s)
	if !ok {
		t.Fatalf("decodeToken(%q): not ok", s)
	}
	return tok
}

// fakeVault is an in-memory VaultClient for tests. List returns the
// keys of Data verbatim (plus any extras in ExtraList for testing
// malformed source-ids); Read returns Data[path] verbatim. Errors can
// be injected per-method.
type fakeVault struct {
	// Data maps base-path-relative keys (e.g. "recognizer") to the
	// KV v2 inner data map (typically {"token": "<hex>"}).
	Data map[string]map[string]any
	// ExtraList augments the ListPath return with values that aren't
	// keys in Data (e.g. to test malformed-source-id handling).
	ExtraList []string
	// ListErr, if non-nil, is returned by ListPath.
	ListErr error
	// ReadErrs maps base-path-relative keys to errors to be returned
	// by ReadKV for that key. Other keys read successfully.
	ReadErrs map[string]error
}

// Compile-time check: fakeVault implements VaultClient.
var _ VaultClient = (*fakeVault)(nil)

// ListPath returns the union of Data's keys and ExtraList, or ListErr.
func (f *fakeVault) ListPath(_ context.Context, _, _ string) ([]string, error) {
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	out := make([]string, 0, len(f.Data)+len(f.ExtraList))
	for k := range f.Data {
		out = append(out, k)
	}
	out = append(out, f.ExtraList...)
	return out, nil
}

// ReadKV returns Data[relativeKey] or the configured per-key error.
// The relative key is derived by stripping basePath+"/" from path.
func (f *fakeVault) ReadKV(_ context.Context, _, path string) (map[string]any, error) {
	// Extract the sourceID portion: ReadKV is called with the full
	// "<basePath>/<sourceID>". Tests use a known basePath, so the
	// trailing segment after the last "/" is the source-id.
	sid := path
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			sid = path[i+1:]
			break
		}
	}
	if err, ok := f.ReadErrs[sid]; ok {
		return nil, err
	}
	if data, ok := f.Data[sid]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("fakeVault: no data for %q", sid)
}
