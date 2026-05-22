# Vault provisioning for spec-13 archive delivery

> **Bead:** [`glovebox-h4wz`](../../.beads/issues.jsonl) (P5 of `glovebox-gdp4`)
> **Prerequisite for:** P6 deploy + smoke test (`glovebox-3d4m`).
> **Time:** ~10 minutes once you have a Vault root token.

This runbook provisions the Vault state the glovebox archive listener
needs at boot: a KV v2 mount, a read-only policy on the ingest-tokens
sub-tree, a K8s auth role binding glovebox's ServiceAccount to that
policy, and one source-id token entry for the smoke test. None of these
steps need to be re-done unless someone tears down Vault.

The chart's defaults (`charts/glovebox/values.yaml` -> `ingest.auth.vault`)
target the values produced by this runbook:

```yaml
ingest:
  auth:
    vault:
      addr: "http://vault.vault.svc.cluster.local:8200"
      k8sRole: "glovebox-ingest"
      tokensPath: "glovebox/ingest-tokens"
      kvMount: "secret"
```

If you deviate from any of these names below, update the Helm overlay
to match.

## 1. Port-forward Vault + login

```bash
kubectl port-forward -n vault svc/vault 8200:8200 &
export VAULT_ADDR=https://localhost:8200
# If the Vault cert is self-signed (likely in the homelab):
export VAULT_SKIP_VERIFY=true
vault login    # paste root token interactively
```

## 2. Confirm the KV v2 mount

```bash
vault secrets list -format=json | jq '."secret/" // {}'
```

If `"type": "kv"` with `"options": {"version": "2"}` shows, skip the
next step. Otherwise enable it:

```bash
vault secrets enable -path=secret kv-v2
```

> Spec 06 / 12 already provision this mount in most setups -- the
> schoology connector reads from it. If the schoology connector is
> running, the mount is already there.

## 3. Write the read-only ingest-tokens policy

```bash
vault policy write glovebox-ingest-read - <<'EOF'
path "secret/data/glovebox/ingest-tokens/*" {
  capabilities = ["read"]
}
path "secret/metadata/glovebox/ingest-tokens" {
  capabilities = ["list"]
}
EOF
```

Spec 10 §4.1 step 7 requires `list` on `secret/metadata/glovebox/ingest-tokens`
so the token-reload goroutine can enumerate every source-id. The read
capability is scoped to `secret/data/glovebox/ingest-tokens/*` so a
compromised glovebox pod cannot exfiltrate other Vault secrets. Do NOT
broaden the read path to `secret/data/glovebox/*` -- that would let
glovebox read the schoology creds.

## 4. Confirm/enable Kubernetes auth + bind the role

```bash
vault auth list | grep kubernetes/
```

If the kubernetes/ auth method isn't enabled:

```bash
vault auth enable kubernetes

# Configure the auth method to talk to the API server. The token-reviewer
# JWT and CA cert come from inside the cluster; this command runs once
# per cluster.
kubectl create serviceaccount vault-token-reviewer -n vault
kubectl create clusterrolebinding vault-token-reviewer \
  --clusterrole=system:auth-delegator \
  --serviceaccount=vault:vault-token-reviewer

REVIEWER_JWT=$(kubectl -n vault create token vault-token-reviewer --duration=8760h)
K8S_HOST=https://kubernetes.default.svc
K8S_CA=$(kubectl get cm -n kube-system kube-root-ca.crt -o jsonpath='{.data.ca\.crt}')

vault write auth/kubernetes/config \
  kubernetes_host="$K8S_HOST" \
  kubernetes_ca_cert="$K8S_CA" \
  token_reviewer_jwt="$REVIEWER_JWT" \
  disable_iss_validation=true
```

Bind the role:

```bash
vault write auth/kubernetes/role/glovebox-ingest \
  bound_service_account_names=glovebox-glovebox \
  bound_service_account_namespaces=glovebox \
  policies=glovebox-ingest-read \
  ttl=24h
```

> Role name `glovebox-ingest` matches `.Values.ingest.auth.vault.k8sRole`.
> The bound SA name is `glovebox-glovebox` (verified via
> `kubectl get sa -n glovebox` after helm install — helm derives the
> name as `{release}-{name}` = `glovebox-glovebox`, not the plain
> `glovebox` an earlier draft of this runbook claimed).

## 5. Generate + write the recognizer's source-id token

```bash
TOKEN=$(openssl rand -hex 32)
echo "Save this -- you'll need it for the smoke test: $TOKEN"
vault kv put secret/glovebox/ingest-tokens/recognizer-smoke-test token="$TOKEN"
```

> Spec 10 §3.2 source-id format: `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, max
> 64 chars. `recognizer-smoke-test` satisfies this. Production
> recognizer should use a different, durable source-id (e.g.
> `recognizer-prod` or `recognizer-v1`); rotate the token via
> `vault kv put` and SIGHUP the glovebox pod to pick it up.

## 6. Verify

```bash
vault kv get secret/glovebox/ingest-tokens/recognizer-smoke-test
vault read auth/kubernetes/role/glovebox-ingest
vault policy read glovebox-ingest-read
```

All three should return without error. Stop the port-forward:

```bash
kill %1
```

## 7. Hand off to the smoke test

Put the source-id token someplace you can read it during the smoke
test (1Password, sticky note, etc.):

```
GLOVEBOX_INGEST_SOURCE_ID=recognizer-smoke-test
GLOVEBOX_INGEST_TOKEN=<the 64-char hex from step 5>
GLOVEBOX_INGEST_URL=http://glovebox.glovebox.svc.cluster.local:9091
                  # or whatever the eventual ingress hostname is
```

Then proceed to bead `glovebox-3d4m` (P6 deploy + smoke).

## Rotating tokens later

To rotate the smoke-test token:

```bash
NEW=$(openssl rand -hex 32)
vault kv put secret/glovebox/ingest-tokens/recognizer-smoke-test token="$NEW"
kubectl rollout restart deploy/glovebox -n glovebox
# OR (faster): kubectl exec deploy/glovebox -- kill -HUP 1
```

Spec 10 §4.1 documents that in-flight uploads complete under a revoked
token; full containment requires the rollout-restart (or SIGHUP +
30-minute drain depending on policy).

## Adding a new consumer source-id

```bash
NEW_TOKEN=$(openssl rand -hex 32)
vault kv put secret/glovebox/ingest-tokens/<new-source-id> token="$NEW_TOKEN"
# Glovebox picks it up within reloadIntervalSeconds (default 300s);
# force-reload with SIGHUP if needed.
```

Add an ExternalSecret in the consumer's namespace pointing at
`secret/glovebox/ingest-tokens/<new-source-id>` so the consumer can
read its own token without a Vault client (see
`charts/glovebox/templates/archive-tokens-externalsecret.yaml` for the
recognizer pattern).
