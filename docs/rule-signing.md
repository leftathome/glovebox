# Signing the ruleset

`rules.json` is the single place where every boundary in glovebox is defined:
the detection rules, their weights, and `quarantine_threshold`. It ships as a
mounted Kubernetes ConfigMap.

That means **anyone with `configmap` edit rights in the glovebox namespace can
disable the product**, and quietly. Raising `quarantine_threshold` to `2.0`
puts it above anything the ruleset can score, so nothing is ever quarantined;
so does deleting the interesting rules or zeroing their weights. Before this
change the daemon would start, log a rule count, and pass everything through to
the agents — with an audit trail full of `PASS` verdicts that look completely
normal.

Two controls already existed and neither is sufficient on its own:

- **Provenance** (`audit/ruleset.jsonl`) records the SHA-256 of the ruleset in
  force. It makes the change *visible after the fact*; it does not prevent it.
- **Digest pinning** (`rules_sha256`) refuses a ruleset that does not match a
  digest carried in the config. It works, but the digest has to be re-copied by
  hand on every legitimate rule change, so in practice it gets set once and
  then abandoned — and it lives in the same ConfigMap as the rules.

Signing moves the trust anchor onto an Ed25519 key that lives **outside the
cluster**. Rules can change as often as their author likes; an edit made by
anyone who does not hold the private key is refused.

## What is signed, and what is not

- The signature is **detached** — a sibling file, `rules.json.sig` by default.
  It cannot live inside `rules.json`, because what is signed is the exact bytes
  of `rules.json`.
- The signed message is `"glovebox-ruleset-v1\n" + <hex sha256 of rules.json> +
  "\n"`. The domain prefix means a glovebox ruleset signature cannot be
  replayed as a signature over a bare digest in some other system, or vice
  versa.
- The signature file may safely sit in the **same ConfigMap** as the rules. An
  attacker who rewrites both still cannot produce a signature that verifies.
- The **public key must not**. It is mounted from a separate Secret precisely
  so that ConfigMap-edit rights do not also grant the ability to swap the
  verification key. Putting the key in the rules ConfigMap would make the whole
  exercise decorative.

## Fail-closed

When `rules_signing.mode` is `permissive` or `required` and verification
**fails**, the process exits. It does not start with the ruleset it could not
verify, and it does not fall back to an earlier one.

This is deliberate, and it is the right trade for this product specifically.
glovebox is the boundary between untrusted content and the agents that act on
it; enforcing rules that may have been written by an attacker is strictly worse
than not running:

- A stopped scanner is **loud and lossless**. Items wait in staging (a
  persistent volume), `/readyz` goes red, connectors get connection errors,
  alerts fire. Nothing is delivered and nothing is destroyed.
- A subverted scanner is **silent and lossy**. Hostile content reaches an agent
  that will act on it, and the audit log says `PASS`. There is no later moment
  at which this gets noticed on its own.

Before exiting, the daemon writes the refusal to `audit/ruleset.jsonl` as an
`event: "ruleset_rejected"` entry carrying the digest of the file it refused
and the reason. A rejection that only reached stderr would be a rejection
nobody could reconstruct afterwards.

`permissive` mode tolerates **an absent signature** with a warning — that is
the rollout state, while the key is deployed but signatures are not yet. It
does not tolerate a signature that fails to verify: a bad signature is the
attack, not a migration state.

## Modes

| `rules_signing.mode` | Signature present and valid | Signature absent | Signature invalid |
|---|---|---|---|
| `disabled` (default) | not checked | not checked | not checked |
| `permissive` | enforced, recorded as verified | boots, warns, recorded **unverified** | **refuses to start** |
| `required` | enforced, recorded as verified | **refuses to start** | **refuses to start** |

Under `disabled` the key file and the signature file are never opened. An
install that has not opted in behaves exactly as it did before this feature
existed.

## Operator flow

The signing tool is `cmd/rules-sign`. It is **not** a shipped binary — it is
absent from `scripts/build-targets.sh` on purpose, so it is in no release
archive and no container image. The signing key belongs on an operator machine
or an offline store, not in the pod that consumes the rules it signs.

```sh
go run ./cmd/rules-sign <subcommand>
# or build it once:
go build -o rules-sign ./cmd/rules-sign
```

### 1. Generate a keypair

```sh
rules-sign keygen \
  -private ~/.glovebox/ruleset-signing.key.pem \
  -public  ~/.glovebox/ruleset-signing.pub
```

`-private` is written mode `0600`. **It must never be committed to this
repository, added to the chart, or placed in CI.** Only the `.pub` half is
deployed. `keygen` refuses to overwrite an existing private key without
`-force`, because overwriting one invalidates every signature made with it and
there is no getting it back.

`openssl` works too, if you would rather:

```sh
openssl genpkey -algorithm ed25519 -out ruleset-signing.key.pem
openssl pkey -in ruleset-signing.key.pem -pubout -out ruleset-signing.pub
```

Both tools produce PKCS#8 / PKIX PEM, and glovebox reads either.

### 2. Sign the ruleset

```sh
rules-sign sign \
  -rules configs/default-rules.json \
  -private ~/.glovebox/ruleset-signing.key.pem
# writes configs/default-rules.json.sig
```

`sign` parses and validates the ruleset first: a signature asserts that these
are the rules you meant to deploy, and signing a file the daemon will refuse to
parse only moves the failure to 3am.

Check it before shipping it — `verify` runs the daemon's own loader, so it
answers "will glovebox accept this?" rather than a similar question:

```sh
rules-sign verify -rules configs/default-rules.json -public ~/.glovebox/ruleset-signing.pub
```

### 3. Deploy the public key as a Secret

```sh
kubectl -n glovebox create secret generic glovebox-rules-signing-key \
  --from-file=rules.pub=$HOME/.glovebox/ruleset-signing.pub
```

Then lock write access down — the point of a separate object is the separate
RBAC:

```yaml
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["glovebox-rules-signing-key"]
  verbs: ["get"]          # and nothing else, for the workload's readers
```

### 4. Turn it on in the chart

A signature covers exact bytes, so the ruleset must be supplied **verbatim**
(`--set-file`), not as structured YAML the chart re-serialises. `config.rulesJson`
goes through `toPrettyJson`, which reorders and reformats; the chart refuses
that combination rather than letting it fail as a digest mismatch in a
CrashLoopBackOff.

```sh
helm upgrade --install glovebox charts/glovebox \
  --set-file rules.json=configs/default-rules.json \
  --set-file rules.signature=configs/default-rules.json.sig \
  --set rules.signing.mode=permissive \
  --set rules.signing.publicKeySecret=glovebox-rules-signing-key
```

The chart renders `rules.json` and `rules.json.sig` into the scanner ConfigMap
and mounts the Secret read-only at `/etc/glovebox-rules-key`.

### 5. Confirm, then move to `required`

```sh
kubectl -n glovebox exec deploy/glovebox -- tail -1 /data/glovebox/audit/ruleset.jsonl
```

```json
{
  "timestamp": "2026-08-21T09:14:02Z",
  "event": "ruleset_loaded",
  "rules_file": "/etc/glovebox/rules.json",
  "pinned": false,
  "rules": {
    "sha256": "…",
    "rule_count": 12,
    "quarantine_threshold": 0.8,
    "max_achievable_score": 8.4,
    "threshold_reachable": true,
    "signature": {
      "mode": "permissive",
      "verified": true,
      "key_fingerprint": "3f2a1c…",
      "signature_file": "/etc/glovebox/rules.json.sig",
      "public_key_file": "/etc/glovebox-rules-key/rules.pub",
      "trusted_keys": 1
    }
  }
}
```

`"verified": true` with the expected `key_fingerprint` means you can set
`rules.signing.mode=required`. The `signature` object is present on **every**
entry, including under `mode: disabled` — "never checked" and "checked and
unverified" must not look the same to whoever reads this log a year from now.

## Rolling a signing key

The public key file may hold **several** keys; a ruleset signed by any of them
verifies. That is what makes a roll possible without a flag day.

1. Generate the new keypair.

   ```sh
   rules-sign keygen -private ruleset-signing-2027.key.pem -public ruleset-signing-2027.pub
   ```

2. Trust **both** keys. Concatenate the PEM files and replace the Secret:

   ```sh
   cat ruleset-signing.pub ruleset-signing-2027.pub > rules.pub
   kubectl -n glovebox create secret generic glovebox-rules-signing-key \
     --from-file=rules.pub --dry-run=client -o yaml | kubectl apply -f -
   ```

   Restart the scanner so it re-reads the key file (the key is read once at
   startup). `rules-sign fingerprint -public rules.pub` prints both
   fingerprints; both should be ones you recognise.

3. Re-sign the ruleset with the **new** key and roll it out.

   ```sh
   rules-sign sign -rules configs/default-rules.json -private ruleset-signing-2027.key.pem
   ```

   Confirm `key_fingerprint` in `ruleset.jsonl` is now the new key's.

4. Retire the old key: drop it from the Secret, leaving one key, and restart.
   Destroy the old private key.

`trusted_keys` in the audit entry is how you notice step 4 never happened — a
deployment that has sat at `2` for months is one that forgot to retire a key.

## Threat model, honestly

What this stops: an attacker with `configmap` edit (or `helm upgrade` rights
against the rules values) silently weakening or emptying the ruleset. That is
the finding in `docs/assessments/2026-08-20/security-review.md`.

What it does not stop:

- **An attacker who can edit the Deployment.** They can set
  `rules_signing.mode` back to `disabled`, change the mount, or replace the
  image. Signing raises the bar from "edit a ConfigMap" to "edit the workload
  spec"; it does not make the pod spec trustworthy. Guard that with RBAC and
  admission policy.
- **An attacker who obtains the private key.** Hence keeping it off the
  cluster, off CI, and out of this repository — and hence the roll procedure
  above.
- **Rollback to an older, legitimately signed ruleset.** The signature carries
  no version or expiry, so a previously valid `rules.json` + `.sig` pair stays
  valid forever. Pair signing with `rules_sha256` if you need to pin one exact
  revision; the audit log records the digest either way, so a rollback is at
  least attributable.
- **A ruleset that is signed and bad.** Signing proves authorship, not
  quality. The reachability check (`threshold_reachable`) and the adversarial
  corpus gate (`scripts/corpus-gate.sh`) are what cover that.

## Configuration reference

`config.json`:

```json
"rules_signing": {
  "mode": "required",
  "public_key_file": "/etc/glovebox-rules-key/rules.pub",
  "signature_file": ""
}
```

| Key | Default | Meaning |
|---|---|---|
| `mode` | `"disabled"` | `disabled`, `permissive`, `required` |
| `public_key_file` | — | Trusted Ed25519 key(s). Required when mode is not `disabled`. |
| `signature_file` | `<rules_file>.sig` | Detached signature path. |

Environment overrides: `GLOVEBOX_RULES_SIGNING_MODE`,
`GLOVEBOX_RULES_PUBLIC_KEY_FILE`, `GLOVEBOX_RULES_SIGNATURE_FILE`.

Helm values: `rules.json`, `rules.signature`, `rules.signing.mode`,
`rules.signing.publicKeySecret`, `rules.signing.publicKeySecretKey`.

### Public key file format

Either PEM `PUBLIC KEY` blocks (what `rules-sign keygen` and `openssl pkey
-pubout` produce), or one standard-base64 raw 32-byte key per line with `#`
comments — and a file may mix the two. Multiple keys are supported for the
rotation window above.

### Signature file format

```json
{
  "algorithm": "ed25519",
  "key_id": "3f2a1c9d4e5b6a70",
  "sha256": "<hex sha256 of rules.json>",
  "signature": "<base64 of the 64-byte Ed25519 signature>"
}
```

`key_id` is the fingerprint of the signing key — the first 16 hex characters of
`sha256(public key)`. It is a hint for operators and for the audit record;
verification tries every trusted key regardless, so a forged `key_id` buys an
attacker nothing.
