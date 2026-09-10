# unifi connector

Delivers UniFi Protect events into glovebox so they reach agents through the
scanning pipeline rather than by a direct path to the openclaw gateway.

Everything below was verified against a UDM Pro Max running Protect 7.3.47 on
2026-09-10, against the published spec at
`https://developer.ui.com/protect/v7.3.47/openapi.json`, and against live event
traffic.

## Transport: a WebSocket subscription, not a poll

**There is no pollable events endpoint.** Verified on the hardware:

| endpoint | result |
|---|---|
| `GET /proxy/protect/integration/v1/events` | 404 `Entity 'endpoint' not found` |
| `GET /proxy/protect/integration/v1/cameras/{id}/events` | 404 |
| `GET /proxy/network/integration/v1/sites/{id}/events` | 404 `No endpoint GET ...` |
| `GET|POST /proxy/network/api/s/{site}/stat/event` | 404 `api.err.NotFound` |
| `GET|POST /proxy/network/api/s/{site}/stat/alarm` | 404 |
| `GET /proxy/protect/api/events` (classic) | 401 -- classic API refuses an API key |

`/stat/sta` answers 200 on the same site path with the same key, so this is not
a wrong-site or wrong-credential problem. The endpoints are simply absent.

What exists is `GET /v1/subscribe/events`, which the spec describes as "A
WebSocket subscription that broadcasts Protect events". The connector implements
`connector.Watcher` against it.

**The upgrade must be made over HTTP/1.1.** The UDM serves HTTP/2, a WebSocket
upgrade cannot be performed over HTTP/2, and the controller reports the failed
attempt as a **404** -- which reads exactly like a missing endpoint and is how
this was originally misdiagnosed.

Authentication is the same read-only `X-API-KEY` used for the REST calls. No
session credential, no username and password: "eyes not hands" is intact.

## The Network surface does not work, and says so

`network.enabled: true` is a **permanent startup error**. On this firmware the
classic event endpoints are gone, and the events WebSocket at
`/proxy/network/wss/s/<site>/events` upgrades at nginx (101) and is then closed
immediately by the application unless the caller holds a session cookie --
verified three times, zero frames received. Delivering Network events would mean
minting a username/password credential, which is a different blast radius and a
decision to take deliberately rather than to discover at runtime.

It fails loudly rather than staging nothing, so an operator learns why instead
of watching an empty directory.

## What an event actually contains

Across all 38 event types in the spec the maximum field set is:

```
id, modelKey, type, start, end (nullable), device,
  plus EITHER smartDetectTypes OR metadata
```

Real frames, captured live:

```json
{"type":"add","item":{"id":"c0b8c646","modelKey":"event","type":"smartDetectZone",
  "start":1789001974452,"device":"699fe724","smartDetectTypes":["person"]}}
{"type":"update","item":{"id":"c0b8c646","type":"smartDetectZone",
  "start":1789001974452,"device":"699fe724","smartDetectTypes":["face","person"],"modelKey":"event"}}
{"type":"add","item":{"id":"23e10ecd","modelKey":"event","type":"motion",
  "start":1789002063195,"device":"6736859600ff"}}
```

Detection vocabularies:

- **video** -- `person`, `vehicle`, `package`, `licensePlate`, `face`, `animal`
- **audio** -- `alrmSmoke`, `alrmCmonx`, `alrmSiren`, `alrmBabyCry`, `alrmSpeak`,
  `alrmBark`, `alrmBurglar`, `alrmCarHorn`, `alrmGlassBreak`

Event families include `ringEvent` (doorbell), `cameraMotionEvent`,
`cameraSmartDetectZone/Line/Loiter/AudioEvent`, the `sensor*` and `alarmHub*`
sets, `nfcCardScannedEvent` and `fingerprintIdentifiedEvent`.

**No bounding box, no thumbnail, no score, no media handle appears in any event
type.** An event tells you what was detected, on which device, and when. To get
pixels you call `GET /v1/cameras/{id}/snapshot` yourself. Media-by-reference is
therefore what the API already does, not a policy this connector adds.

## One event, several frames

Protect refines a detection while it is still happening. The capture above shows
one person producing an `add` with `["person"]` and then updates with
`["face","person"]`, all under one `item.id`, with `end` absent until the event
closes.

Staging every frame would deliver three items for one event, the first claiming
a person when a face was identified moments later. So frames are merged on
`item.id` and the event is staged **once, when it ends**.

An event whose `end` never arrives -- a dropped subscription, or a Protect
event left open -- is staged after `maxOpenAge` (5 minutes) and tagged
`unifi.incomplete=true`. Late and labelled beats silently dropped.

## Channel tier

Declares **`TierFeed`**, and that is the load-bearing decision.

UniFi event volume is far above RSS, and RSS alone measured 89% of the main
agent's memory index and effectively 100% of one person-agent's before diversion
existed. `TierFeed` makes openclaw's triage divert these to caro's feed store
instead of `audiences/<group>/inbox/`, where they would enter every
person-agent's ambient recall. Agents reach them deliberately via `search_items`.

Changing it is a code change and a redeploy, on purpose. See `connector/tier.go`
and connector-guide section 3.7.

## Threat model

The UDM is a trusted reporter of untrusted observations. The Protect event
schema is narrow and mostly enums, but `metadata` on the sensor, alarm-hub, NFC
and fingerprint families is free-form, and `licensePlate` and `detectedName` are
read off objects and people the observer does not control.

Every field reaches `content.raw`, because that is what the scanner reads. Items
are tagged `unifi.untrusted_fields` naming which attacker-settable channels were
populated. The subject is built from the surface name and a
character-constrained event kind only -- attacker text in a subject would still
be scanned, but the subject is what a human sees first in a quarantine review
and should not repeat an attacker's sentence back as though glovebox wrote it.

Inlined media payloads are replaced with a placeholder naming what was removed
and recorded in `unifi.stripped_media`. Only long **strings** are treated as
payloads: an object or array is structure, and destroying a bounding box because
its key was `image` was a real bug (`glovebox-ext6`).

## Authentication

**Controller (required).** A read-only UniFi API key, sent as `X-API-KEY`. Reuse
the existing key at `external-dns/external-dns-unifi-secret`; do not mint a write
credential. Set `api_key_env` (default `UNIFI_API_KEY`).

**TLS.** A UDM presents a self-signed certificate for its LAN address:

- `ca_cert_file` -- a PEM bundle containing the controller's certificate. Preferred.
- system trust -- leave both unset.
- `insecure_skip_verify: true` -- verification off; the API key is then the only
  thing authenticating the controller. The sample config's default, because it is
  the common home case.

Mutually exclusive; the connector refuses to start with both.

**Webhook (optional).** The listener is fail-closed: with no secret configured it
refuses every request with 503, because staging is a write channel into agent
context. Modes are `hmac` (SHA-256 over the body) and `bearer` (constant-time
shared secret).

## Configuration

| field | default | meaning |
|---|---|---|
| `controller_url` | -- | required; `http://` or `https://` |
| `site` | `default` | classic-API site id (the integration API uses a UUID) |
| `api_key_env` | `UNIFI_API_KEY` | env var holding the read-only key |
| `ca_cert_file` | -- | PEM bundle for the controller certificate |
| `insecure_skip_verify` | `false` | disable TLS verification |
| `webhook_secret_env` | -- | env var holding the webhook secret |
| `webhook_auth_mode` | `hmac` | `hmac` or `bearer` |
| `protect.enabled` | `false` | enable the Protect subscription |
| `protect.events_path` | `/proxy/protect/integration/v1/subscribe/events` | subscription path |
| `network.enabled` | `false` | **unsupported**; enabling it is a startup error |

## Routing

Rules are looked up most specific first: `event:<kind>` (e.g.
`event:smartDetectZone`, `event:ring`, `event:motion`), then `<surface>`
(`protect`), then `*`. Nothing is staged when no rule matches.

Destinations must appear in `scanner.agentAllowlist` or the scanner drops the
item after scanning it.

| tag | value |
|---|---|
| `unifi.surface` | `protect` |
| `unifi.kind` | the constrained event kind |
| `unifi.via` | `subscribe` or `webhook` |
| `unifi.frames` | how many frames were merged into this item |
| `unifi.incomplete` | `true` when staged without an `end` |
| `unifi.untrusted_fields` | populated attacker-settable fields |
| `unifi.stripped_media` | media fields whose payload was removed |

Content type is `application/vnd.unifi.event+json`.

## Readiness

`Poll` does not fetch events -- there is nothing to fetch. It calls
`GET /v1/meta/info`, which confirms the controller is reachable and the
credential still works, and that is what turns `/readyz` green. A rejected key is
a permanent error.

## Enabling in the Helm chart

```yaml
connectors:
  unifi:
    enabled: true
    listener:
      enabled: true      # only needed if you also use webhook delivery
    secrets: unifi-connector-secrets   # provides UNIFI_API_KEY
    config:
      controller_url: "https://192.168.1.1"
      protect:
        enabled: true
      network:
        enabled: false
```

## Related, not duplicate

`gitops-kgaf` and `gitops-cp88` push UniFi telemetry into Prometheus/Grafana for
historical querying and alerting. This connector delivers events into agent
context via caro. Complementary paths, different consumers.
