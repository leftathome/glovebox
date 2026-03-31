package connector

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// VerifyHMAC verifies an HMAC signature over payload using the given secret
// and algorithm. Supports hex-encoded and base64-encoded signatures.
// Handles GitHub-style "sha256=" prefixes. Uses constant-time comparison.
func VerifyHMAC(payload []byte, signature string, secret []byte, algo string) bool {
	if algo != "sha256" {
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	expected := mac.Sum(nil)

	// Strip algorithm prefix if present (e.g. "sha256=...").
	sig := signature
	if idx := strings.Index(sig, "="); idx >= 0 && idx < 10 {
		prefix := sig[:idx]
		if prefix == "sha256" {
			sig = sig[idx+1:]
		}
	}

	// Try hex decoding first.
	decoded, err := hex.DecodeString(sig)
	if err == nil {
		return hmac.Equal(decoded, expected)
	}

	// Try base64 decoding.
	decoded, err = base64.StdEncoding.DecodeString(sig)
	if err == nil {
		return hmac.Equal(decoded, expected)
	}

	return false
}
