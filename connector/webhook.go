package connector

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// VerifyHMAC verifies an HMAC signature over payload using the given secret
// and algorithm. Supports hex-encoded and base64-encoded signatures (hex is
// tried first). Handles GitHub-style "sha256=" prefixes. Uses constant-time
// comparison via hmac.Equal.
func VerifyHMAC(payload []byte, signature string, secret []byte, algo string) bool {
	if algo != "sha256" {
		return false
	}

	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	expected := mac.Sum(nil)

	sig := signature
	if after, found := strings.CutPrefix(sig, "sha256="); found {
		sig = after
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
