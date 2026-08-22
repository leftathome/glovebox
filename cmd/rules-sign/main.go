// Command rules-sign generates ruleset signing keys, signs a rules file,
// and verifies a signature — the operator half of `rules_signing` in
// internal/config.
//
// A security control nobody can operate is not a security control, and
// "sign the ruleset" is not a thing an operator can do by hand: the
// message is domain-separated and the envelope has a shape. This tool is
// that shape.
//
// It is NOT a shipped component. scripts/build-targets.sh discovers
// binaries under connectors/ and importers/ only, so nothing here reaches
// a release archive or a container image, which is deliberate: the
// signing key belongs on an operator's machine or in an offline HSM, not
// in the pod that consumes the rules it signs. Shipping the signer in the
// glovebox image would put the tooling for forging a ruleset in the same
// place as the thing it protects.
//
// Usage:
//
//	rules-sign keygen  -private ruleset-signing.key.pem -public ruleset-signing.pub
//	rules-sign sign    -rules configs/default-rules.json -private ruleset-signing.key.pem [-out rules.json.sig]
//	rules-sign verify  -rules configs/default-rules.json -public ruleset-signing.pub [-sig rules.json.sig]
//	rules-sign fingerprint -public ruleset-signing.pub
//
// See docs/rule-signing.md.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"os"

	"github.com/leftathome/glovebox/internal/engine"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	case "fingerprint":
		err = fingerprint(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rules-sign: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rules-sign — Ed25519 signing for the glovebox ruleset

  rules-sign keygen -private KEY.pem -public KEY.pub
  rules-sign sign -rules rules.json -private KEY.pem [-out rules.json.sig]
  rules-sign verify -rules rules.json -public KEY.pub [-sig rules.json.sig]
  rules-sign fingerprint -public KEY.pub

The private key never belongs in this repository, in the Helm chart, or in
CI. Only the .pub half is deployed. See docs/rule-signing.md.
`)
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	privPath := fs.String("private", "", "path to write the PKCS#8 PEM private key (required)")
	pubPath := fs.String("public", "", "path to write the PKIX PEM public key (required)")
	force := fs.Bool("force", false, "overwrite an existing private key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *privPath == "" || *pubPath == "" {
		fs.Usage()
		return fmt.Errorf("keygen requires -private and -public")
	}
	// Refuse to clobber a signing key by accident: overwriting it
	// silently invalidates every signature made with it, and there is no
	// recovering the old one.
	if _, err := os.Stat(*privPath); err == nil && !*force {
		return fmt.Errorf("%s already exists; pass -force to overwrite (this destroys the old signing key)", *privPath)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	privPEM, err := engine.MarshalPrivateKey(priv)
	if err != nil {
		return err
	}
	pubPEM, err := engine.MarshalPublicKey(pub)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*privPath, privPEM, 0o600); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	if err := os.WriteFile(*pubPath, pubPEM, 0o644); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	fmt.Printf("private key: %s (mode 0600)\n", *privPath)
	fmt.Printf("public key:  %s\n", *pubPath)
	fmt.Printf("fingerprint: %s\n", engine.KeyFingerprint(pub))
	fmt.Fprintf(os.Stderr, "\nKeep %s off this repository, off the chart and out of CI. Only %s is deployed.\n", *privPath, *pubPath)
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	rulesPath := fs.String("rules", "", "rules file to sign (required)")
	privPath := fs.String("private", "", "PKCS#8 PEM private key (required)")
	outPath := fs.String("out", "", "signature output path (default <rules>.sig)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *rulesPath == "" || *privPath == "" {
		fs.Usage()
		return fmt.Errorf("sign requires -rules and -private")
	}

	raw, err := os.ReadFile(*rulesPath)
	if err != nil {
		return fmt.Errorf("read rules: %w", err)
	}
	// Parse before signing. A signature is an assertion that these rules
	// are the ones the operator meant to deploy; signing a file the
	// daemon will refuse to parse just moves the failure to 3am.
	if _, err := engine.LoadRulesBytes(raw); err != nil {
		return fmt.Errorf("%s is not a valid ruleset, refusing to sign it: %w", *rulesPath, err)
	}
	privPEM, err := os.ReadFile(*privPath)
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}
	priv, err := engine.ParsePrivateKey(privPEM)
	if err != nil {
		return err
	}
	env, err := engine.SignRuleset(raw, priv)
	if err != nil {
		return err
	}
	out, err := engine.MarshalSignature(env)
	if err != nil {
		return err
	}
	dest := *outPath
	if dest == "" {
		dest = *rulesPath + ".sig"
	}
	if err := os.WriteFile(dest, out, 0o644); err != nil {
		return fmt.Errorf("write signature: %w", err)
	}
	fmt.Printf("signed %s\n", *rulesPath)
	fmt.Printf("  sha256:      %s\n", env.SHA256)
	fmt.Printf("  key:         %s\n", env.KeyID)
	fmt.Printf("  signature:   %s\n", dest)
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	rulesPath := fs.String("rules", "", "rules file to verify (required)")
	pubPath := fs.String("public", "", "trusted public key file (required)")
	sigPath := fs.String("sig", "", "signature path (default <rules>.sig)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *rulesPath == "" || *pubPath == "" {
		fs.Usage()
		return fmt.Errorf("verify requires -rules and -public")
	}
	policy := engine.SignaturePolicy{
		Mode:          engine.SigModeRequired,
		PublicKeyFile: *pubPath,
		SignatureFile: *sigPath,
	}
	// Run the daemon's own loader so this answers the question that
	// matters — "will glovebox accept this?" — rather than a similar one.
	_, prov, err := engine.LoadRulesFileWithPolicy(*rulesPath, policy)
	if err != nil {
		return err
	}
	fmt.Printf("OK  %s\n", *rulesPath)
	fmt.Printf("  sha256:      %s\n", prov.SHA256)
	fmt.Printf("  signed by:   %s\n", prov.Signature.KeyFingerprint)
	fmt.Printf("  trusted keys: %d (%s)\n", prov.Signature.TrustedKeys, *pubPath)
	fmt.Printf("  rules:       %d, quarantine threshold %.2f\n", prov.RuleCount, prov.QuarantineThreshold)
	return nil
}

func fingerprint(args []string) error {
	fs := flag.NewFlagSet("fingerprint", flag.ExitOnError)
	pubPath := fs.String("public", "", "public key file (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pubPath == "" {
		fs.Usage()
		return fmt.Errorf("fingerprint requires -public")
	}
	keys, err := engine.LoadPublicKeys(*pubPath)
	if err != nil {
		return err
	}
	for _, k := range keys {
		fmt.Println(k.Fingerprint)
	}
	return nil
}
