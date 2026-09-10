# unifi connector

Delivers UniFi Dream Machine events -- Network and Protect -- into glovebox so
they reach agents through the scanning pipeline rather than by a direct path to
the openclaw gateway.

The connector implements both `Connector` and `Listener`: `Poll` backfills from
the controller API on start and on the poll interval, and the listener receives
webhook pushes in between. It runs as a Deployment.

## Channel tier

This connector declares **`TierFeed`**, and that is the load-bearing decision.

UniFi event volume is far above RSS, and RSS alone was measured at 89% of the
main agent's memory index and effectively 100% of one person-agent's before
diversion existed. `TierFeed` is what makes openclaw's triage divert these items
to caro's feed store instead of writing them into `audiences/<group>/inbox/`,
where they would enter every person-agent's ambient recall. Agents reach UniFi
events deliberately, through the `search_items` MCP tool.

Changing this is a code change and a redeploy, on purpose. See
`connector/tier.go` and the connector guide section 3.7.

## Threat model: the UDM is a trusted reporter of untrusted observations

The controller is authenticated and its TLS is verified (or its certificate is
pinned as a CA bundle). None of that says anything about the *contents* of an
event.

These fields are settable by third parties:

| field | who can set it |
|---|---|
| `hostname` | any DHCP client; devices name themselves |
| `name` | client/device alias |
| `essid` / `ssid` | **anyone within radio range** -- a neighbouring or rogue AP broadcasts whatever SSID it likes, with no access to your network at all |
| `msg` | interpolates the above into a sentence |
| `licensePlate` | read off a physical plate the camera does not control |
| `detectedName` | face-recognition label |

So a structurally valid, authenticated, TLS-verified UniFi event can carry
attacker-authored text. That is exactly why these events go through the scanner
instead of straight to an agent.

Every field reaches `content.raw`, because `content.raw` is what the scanner
reads. Nothing is filtered on the way in. The connector additionally tags each
item with `unifi.untrusted_fields`, naming which hostile channels were populated,
so a reviewer looking at a quarantined item knows which field to read first.

The item `subject` is built from the surface name and a character-constrained
event kind only. Attacker text in the subject would still be scanned, but the
subject is what a human sees first in a quarantine review and it should not
repeat an attacker's sentence back as though glovebox had written it.

## Media does not transit glovebox

Protect clips, audio and stills stay at rest on the controller. The staged event
carries a *reference* -- a short id or URL -- and an agent that needs the footage
reaches for it deliberately.

Three reasons:

1. The scan engine is text pattern matching. An h.264 clip passes through it as
   opaque bytes that no rule can match, and it would emerge with a clean verdict
   having been checked by nothing. A green verdict on unscannable bytes is worse
   than no verdict, because downstream it is indistinguishable from a real one.
2. There is no wire format for it. The ingest path's `mediaAllowList` is six
   `archive/*` types; anything else is `400 unknown_media_type`.
3. Spec 14 §2.2 defers speech-to-text and vision captions, so no enricher would
   turn a clip into scannable text today.

If an event arrives with media inlined anyway, the connector replaces the
payload with a placeholder naming what was removed and records the field in the
`unifi.stripped_media` tag. Short references are left alone.

**Stills are a live follow-up, not a settled no.** A snapshot *is* scannable --
the OCR enricher would extract text into `content.extracted.md`, which is then
scanned for real. That also makes it an injection vector (someone in camera view
holding a sign). It needs its own threat budget and is tracked separately.

## Authentication

**Controller (required).** A read-only UniFi API key, sent as `X-API-KEY`. Reuse
the existing key at `external-dns/external-dns-unifi-secret`; do not mint a write
credential. The connector only ever issues GETs -- "eyes not hands" names the
UniFi controller explicitly, and modifying UniFi rules is forbidden.

Set `api_key_env` to the environment variable holding it (default
`UNIFI_API_KEY`).

**TLS.** A UDM presents a self-signed certificate for its LAN address, so you
have three explicit options:

- `ca_cert_file`: a PEM bundle containing the controller's certificate. Preferred.
- system trust: leave both unset.
- `insecure_skip_verify: true`: verification off, the API key is then the only
  thing authenticating the controller. Honest, and the sample config's default
  because it is the common home case.

`ca_cert_file` and `insecure_skip_verify` are mutually exclusive and the
connector refuses to start with both.

**Webhook (required for the listener).** The listener is **fail-closed**: with no
secret configured it refuses every request with 503. A connector's staging
directory is a write channel into agent context, and an unauthenticated endpoint
on it would let anyone who can reach the pod put content in front of an agent.

Two modes, because UniFi's own behaviour differs by surface and firmware:

- `hmac` (default): a SHA-256 HMAC over the request body, in
  `webhook_signature_header` (default `X-Unifi-Signature`).
- `bearer`: a shared secret compared in constant time, default header
  `Authorization`, `Bearer ` prefix tolerated. Protect's alarm-manager webhooks
  have historically posted plain JSON with no signature, which leaves a shared
  secret as the only available control.

## Configuration

| field | default | meaning |
|---|---|---|
| `controller_url` | -- | required; must start with `http://` or `https://` |
| `site` | `default` | UniFi Network site id |
| `api_key_env` | `UNIFI_API_KEY` | env var holding the read-only key |
| `ca_cert_file` | -- | PEM bundle for the controller certificate |
| `insecure_skip_verify` | `false` | disable TLS verification |
| `webhook_secret_env` | -- | env var holding the webhook secret |
| `webhook_auth_mode` | `hmac` | `hmac` or `bearer` |
| `webhook_signature_header` | `X-Unifi-Signature` | header carrying signature/secret |
| `network.enabled` | `false` | enable the Network surface |
| `network.events_path` | `/proxy/network/api/s/%s/stat/event` | printf template; exactly one `%s` for the site |
| `network.backfill_limit` | `200` | max events one catch-up poll will stage |
| `protect.enabled` | `false` | enable the Protect surface |
| `protect.events_path` | `/proxy/protect/api/events` | |
| `protect.backfill_limit` | `200` | |

At least one surface must be enabled or the connector refuses to start.

Endpoint paths are configuration rather than constants because the UDM exposes
several generations of API surface behind the same reverse proxy, and which one
a given firmware serves is a property of the box. An operator on a firmware that
moved a route corrects it in a ConfigMap instead of waiting for a release.

## Routing

Rules are looked up most specific first:

1. `event:<kind>` -- the firmware event enum, e.g. `event:EVT_AP_RogueAp` or
   `event:smartDetectZone`
2. `<surface>` -- `network` or `protect`
3. `*`

Nothing is staged when no rule matches: routing is the operator's decision and an
unrouted item has nowhere to go.

Destinations must appear in `scanner.agentAllowlist`, or the scanner drops the
item after scanning it.

Each item carries these tags:

| tag | value |
|---|---|
| `unifi.surface` | `network` or `protect` |
| `unifi.kind` | the constrained event kind |
| `unifi.via` | `poll` or `webhook` |
| `unifi.untrusted_fields` | populated attacker-settable fields, comma separated |
| `unifi.stripped_media` | media fields whose payload was removed |

Content type is `application/vnd.unifi.event+json`, narrow so the ruleset can
target this payload shape rather than every `application/json` item.

## Enabling in the Helm chart

```yaml
connectors:
  unifi:
    enabled: true
    listener:
      enabled: true      # adds the HealthPort+1 webhook port to Deployment + Service
    secrets: unifi-connector-secrets   # must provide UNIFI_API_KEY and UNIFI_WEBHOOK_SECRET
    config:
      controller_url: "https://192.168.1.1"
      network:
        enabled: true
      protect:
        enabled: true
```

`listener.enabled` is opt-in per connector: a poll-only connector that advertised
the port would publish a Service target that answers nothing.

Point the UDM's webhook at `http://<release>-unifi.<namespace>.svc:8081/network`
or `/protect`. A push to a surface that is not enabled returns 404.

## Verification status

Unit and component tests run against mock controllers and cover the tier
declaration, the untrusted-field path, media stripping, checkpointing and every
webhook authentication branch.

**Not yet verified against hardware.** The exact event-payload shapes, the
webhook signature header UniFi actually sends, and the endpoint paths on this
firmware have not been confirmed against the real UDM Pro Max. Both response
shapes (`{"data": [...]}` and a bare array) are accepted, and paths and header
names are configuration, specifically so that a mismatch is a values edit rather
than a code change. Confirming these on hardware is the remaining step.

## Related, not duplicate

`gitops-kgaf` and `gitops-cp88` push UniFi telemetry into Prometheus/Grafana for
historical querying and alerting. This connector delivers events into agent
context via caro. Complementary paths, different consumers.
