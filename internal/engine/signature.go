package engine

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Signature verification for the ruleset.
//
// Digest pinning (rules_sha256) already turns an unreviewed edit of the
// rules ConfigMap into a failed start -- but only for a deployment that
// carries the digest in its own config, and the digest has to be updated
// by hand on every legitimate rule change. A signature moves the trust
// anchor off the file being checked and onto a key: the rules may change
// as often as their author likes, and an edit made by anyone who does not
// hold the private key is still rejected.
//
// The signature is DETACHED -- a sibling file, by default
// <rules_file>.sig. It cannot live inside rules.json, because the thing
// being signed is the exact bytes of rules.json; embedding the signature
// would change them.
//
// The signature file may safely sit in the same ConfigMap as the rules it
// covers: an attacker who rewrites both still cannot produce a signature
// that verifies without the private key, and the private key is
// deliberately not anything the cluster holds. The PUBLIC key is
// different -- see SignaturePolicy.PublicKeyFile.

// Signature modes, mirroring the disabled/permissive/required tri-state
// IngestTLSConfig uses for the same reason: a security control that can
// only be off or fully on has no migration path, so it gets switched on
// late or never.
const (
	// SigModeDisabled performs no verification at all. The zero value,
	// so an install that has configured nothing behaves exactly as it
	// did before signing existed: no key is read, no signature file is
	// looked for, nothing can fail.
	SigModeDisabled = "disabled"
	// SigModePermissive verifies a signature when one is present and
	// refuses the ruleset when that verification FAILS, but tolerates a
	// missing signature file with a warning. It is the rollout state:
	// deploy the public key, watch audit/ruleset.jsonl report verified
	// rulesets, then move to required. A bad signature is fatal even
	// here -- "permissive" covers the absence of a signature, never a
	// signature that does not check out, which is the attack.
	SigModePermissive = "permissive"
	// SigModeRequired demands a signature that verifies against a
	// trusted key. Anything else -- absent signature, absent key file,
	// wrong key, tampered rules -- refuses the ruleset.
	SigModeRequired = "required"
)

// sigAlgorithm is the only algorithm accepted. It is written into the
// envelope and checked on the way back in, so a second algorithm cannot
// later be introduced by an attacker downgrading this one.
const sigAlgorithm = "ed25519"

// sigDomain prefixes every signed message.
//
// Ed25519 signs an arbitrary byte string, so without a domain prefix a
// signature this project produced over a ruleset digest would also be a
// valid signature over the same 64 ASCII bytes in any other context that
// signs bare hex with the same key, and vice versa. The prefix (and the
// version in it) makes a glovebox ruleset signature mean one thing only.
const sigDomain = "glovebox-ruleset-v1"

// SigningMessage returns the exact bytes a ruleset signature covers.
//
// Signing the digest rather than the file itself keeps the message a
// fixed size regardless of ruleset size and makes the tooling trivially
// checkable by hand, while binding the signature to the content just as
// tightly: forging a ruleset that verifies under someone else's signature
// would require a SHA-256 preimage.
func SigningMessage(digestHex string) []byte {
	return []byte(sigDomain + "\n" + strings.ToLower(digestHex) + "\n")
}

// SignatureEnvelope is the on-disk form of a detached ruleset signature.
type SignatureEnvelope struct {
	// Algorithm is always "ed25519".
	Algorithm string `json:"algorithm"`
	// KeyID is the fingerprint of the public key that should verify
	// this signature. It is a HINT, for operator legibility and for the
	// audit record; verification tries every trusted key regardless, so
	// a forged key_id buys an attacker nothing.
	KeyID string `json:"key_id"`
	// SHA256 is the hex digest of the ruleset the signature covers.
	// Cross-checked against the file actually loaded before the
	// signature is even considered, so a signature lifted from a
	// different (validly signed) ruleset is rejected here.
	SHA256 string `json:"sha256"`
	// Signature is standard-base64 of the 64-byte Ed25519 signature
	// over SigningMessage(SHA256).
	Signature string `json:"signature"`
}

// TrustedKey is one Ed25519 public key the deployment trusts to sign
// rulesets, with the fingerprint used to name it in logs.
type TrustedKey struct {
	Fingerprint string
	Key         ed25519.PublicKey
}

// KeyFingerprint names a public key: the first 16 hex characters of the
// SHA-256 of its 32 raw bytes. Derived rather than operator-chosen so two
// deployments cannot disagree about what a given key is called, and so an
// attacker cannot pick a label that reads like a trusted key's.
func KeyFingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:16]
}

// ParsePublicKeys reads a trusted-keys file.
//
// Two formats are accepted, and a file may mix them:
//
//   - PEM "PUBLIC KEY" blocks, i.e. exactly what
//     `openssl genpkey -algorithm ed25519 | openssl pkey -pubout`
//     produces, so an operator who would rather not run our tool need not.
//   - One key per line as standard-base64 of the 32 raw public-key
//     bytes. Blank lines and lines beginning with # are ignored.
//
// Multiple keys is the point, not a convenience: rolling a signing key
// means trusting the old and the new key at once for one deploy window
// (see docs/rule-signing.md).
func ParsePublicKeys(raw []byte) ([]TrustedKey, error) {
	var keys []TrustedKey
	seen := map[string]bool{}

	add := func(pub ed25519.PublicKey) {
		fp := KeyFingerprint(pub)
		if seen[fp] {
			return
		}
		seen[fp] = true
		keys = append(keys, TrustedKey{Fingerprint: fp, Key: pub})
	}

	// PEM blocks first; whatever is left over is treated as line format.
	rest := raw
	for {
		block, remainder := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remainder
		if block.Type != "PUBLIC KEY" {
			return nil, fmt.Errorf("public key file: unexpected PEM block %q (want PUBLIC KEY)", block.Type)
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("public key file: parse PEM block: %w", err)
		}
		pub, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("public key file: PEM block is %T, want ed25519.PublicKey", parsed)
		}
		add(pub)
	}

	for i, line := range strings.Split(string(rest), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return nil, fmt.Errorf("public key file line %d: neither PEM nor base64: %w", i+1, err)
		}
		if len(decoded) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("public key file line %d: %d bytes, want %d", i+1, len(decoded), ed25519.PublicKeySize)
		}
		add(ed25519.PublicKey(decoded))
	}

	if len(keys) == 0 {
		return nil, errors.New("public key file contains no ed25519 public keys")
	}
	return keys, nil
}

// LoadPublicKeys reads and parses a trusted-keys file from disk.
func LoadPublicKeys(path string) ([]TrustedKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read rules public key file %s: %w", path, err)
	}
	return ParsePublicKeys(raw)
}

// ParseSignatureEnvelope decodes a detached signature file.
func ParseSignatureEnvelope(raw []byte) (SignatureEnvelope, error) {
	var env SignatureEnvelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return SignatureEnvelope{}, fmt.Errorf("parse ruleset signature: %w", err)
	}
	if env.Algorithm != sigAlgorithm {
		return SignatureEnvelope{}, fmt.Errorf("ruleset signature: algorithm %q is not supported (want %q)", env.Algorithm, sigAlgorithm)
	}
	if len(env.SHA256) != 64 {
		return SignatureEnvelope{}, fmt.Errorf("ruleset signature: sha256 field is %d characters, want 64", len(env.SHA256))
	}
	if _, err := hex.DecodeString(env.SHA256); err != nil {
		return SignatureEnvelope{}, fmt.Errorf("ruleset signature: sha256 field is not hex: %w", err)
	}
	return env, nil
}

// VerifyRuleset checks a detached signature over rules bytes against a set
// of trusted keys, returning the fingerprint of the key that verified.
//
// Order matters. The digest of the bytes actually loaded is computed
// first and compared with the envelope's, so a signature validly issued
// for some OTHER ruleset cannot be pointed at this one; only then is the
// signature itself checked.
func VerifyRuleset(raw []byte, sig SignatureEnvelope, keys []TrustedKey) (string, error) {
	actual := RulesDigest(raw)
	if !strings.EqualFold(actual, sig.SHA256) {
		return "", fmt.Errorf("ruleset signature covers sha256 %s but the rules file is %s", strings.ToLower(sig.SHA256), actual)
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return "", fmt.Errorf("ruleset signature: signature field is not base64: %w", err)
	}
	if len(sigBytes) != ed25519.SignatureSize {
		return "", fmt.Errorf("ruleset signature: %d bytes, want %d", len(sigBytes), ed25519.SignatureSize)
	}
	if len(keys) == 0 {
		return "", errors.New("ruleset signature: no trusted public keys configured")
	}
	msg := SigningMessage(actual)
	for _, k := range keys {
		if ed25519.Verify(k.Key, msg, sigBytes) {
			return k.Fingerprint, nil
		}
	}
	return "", fmt.Errorf("ruleset signature does not verify against any of the %d trusted key(s) (signature names key %s)", len(keys), sig.KeyID)
}

// SignRuleset produces a detached signature envelope over rules bytes.
// Used by cmd/rules-sign and by the tests; the daemon never signs.
func SignRuleset(raw []byte, priv ed25519.PrivateKey) (SignatureEnvelope, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return SignatureEnvelope{}, fmt.Errorf("signing key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	digest := RulesDigest(raw)
	sig := ed25519.Sign(priv, SigningMessage(digest))
	return SignatureEnvelope{
		Algorithm: sigAlgorithm,
		KeyID:     KeyFingerprint(priv.Public().(ed25519.PublicKey)),
		SHA256:    digest,
		Signature: base64.StdEncoding.EncodeToString(sig),
	}, nil
}

// MarshalSignature renders an envelope for writing to disk.
func MarshalSignature(env SignatureEnvelope) ([]byte, error) {
	out, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// ParsePrivateKey reads an Ed25519 private key in PKCS#8 PEM form -- the
// output of both `rules-sign keygen` and `openssl genpkey -algorithm
// ed25519`.
func ParsePrivateKey(raw []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("private key: no PEM block found")
	}
	if block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("private key: unexpected PEM block %q (want PRIVATE KEY)", block.Type)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is %T, want ed25519.PrivateKey", parsed)
	}
	return priv, nil
}

// MarshalPrivateKey renders an Ed25519 private key as PKCS#8 PEM.
func MarshalPrivateKey(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// MarshalPublicKey renders an Ed25519 public key as PKIX PEM.
func MarshalPublicKey(pub crypto.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
