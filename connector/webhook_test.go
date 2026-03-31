package connector

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// helper: compute HMAC-SHA256 of payload with secret and return hex string.
func computeHMACSHA256Hex(payload, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyHMAC_ValidHexSignature(t *testing.T) {
	payload := []byte("hello world")
	secret := []byte("supersecret")
	sig := computeHMACSHA256Hex(payload, secret)

	if !VerifyHMAC(payload, sig, secret, "sha256") {
		t.Error("expected valid hex signature to pass verification")
	}
}

func TestVerifyHMAC_InvalidSignature(t *testing.T) {
	payload := []byte("hello world")
	secret := []byte("supersecret")

	if VerifyHMAC(payload, "deadbeef", secret, "sha256") {
		t.Error("expected invalid signature to be rejected")
	}
}

func TestVerifyHMAC_GitHubStylePrefix(t *testing.T) {
	payload := []byte(`{"action":"opened"}`)
	secret := []byte("webhook-secret")
	sig := "sha256=" + computeHMACSHA256Hex(payload, secret)

	if !VerifyHMAC(payload, sig, secret, "sha256") {
		t.Error("expected GitHub-style sha256= prefixed signature to pass")
	}
}

func TestVerifyHMAC_Base64Signature(t *testing.T) {
	payload := []byte("base64 test payload")
	secret := []byte("b64secret")

	mac := hmac.New(sha256.New, secret)
	mac.Write(payload)
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !VerifyHMAC(payload, sig, secret, "sha256") {
		t.Error("expected base64-encoded signature to pass verification")
	}
}

func TestVerifyHMAC_UnsupportedAlgorithm(t *testing.T) {
	payload := []byte("test")
	secret := []byte("secret")
	sig := computeHMACSHA256Hex(payload, secret)

	if VerifyHMAC(payload, sig, secret, "sha512") {
		t.Error("expected unsupported algorithm to return false")
	}
}

func TestVerifyHMAC_EmptyPayload(t *testing.T) {
	payload := []byte{}
	secret := []byte("emptysecret")
	sig := computeHMACSHA256Hex(payload, secret)

	if !VerifyHMAC(payload, sig, secret, "sha256") {
		t.Error("expected empty payload with valid signature to pass")
	}
}

func TestVerifyHMAC_TamperedPayload(t *testing.T) {
	payload := []byte("original content")
	secret := []byte("tamper-secret")
	sig := computeHMACSHA256Hex(payload, secret)

	tampered := []byte("modified content")
	if VerifyHMAC(tampered, sig, secret, "sha256") {
		t.Error("expected tampered payload to be rejected")
	}
}
