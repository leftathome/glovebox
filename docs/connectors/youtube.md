# youtube connector

Follows YouTube channels and stages one markdown item per new upload: title,
watch URL, thumbnail, description, chapters, labels, relative engagement,
top comments and a transcript marker. Video files are never fetched. Reads
public data with a YouTube Data API v3 key; no OAuth, no personal data.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-youtube` |
| Credential class | `test-account` (YouTube Data API key), see [integration-credentials.md](integration-credentials.md) |
| Tier | `feed` (triage diverts items to caro, out of per-agent recall) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet |

## Authentication

1. In the Google Cloud console, create a project and enable **YouTube Data API v3**.
2. Create an API key and restrict it to that API.
3. Store it in Vault (orac: `eso/glovebox-youtube`, property `YOUTUBE_API_KEY`)
   and sync it with an ESO `ExternalSecret`.

The connector reads `YOUTUBE_API_KEY` from the environment and exits non-zero
if it is unset. The key travels as a query parameter; request errors are
reported without the URL so it cannot reach the logs.

## How it polls, and what it costs

Quota costs below were verified on 2026-11-07 against
<https://developers.google.com/youtube/v3/determine_quota_cost>. The default
quota is 10,000 units/day shared, plus a separate 100 calls/day for
`search.list`. There is no charge; more quota is requested through Google's
Quota Extension form, which comes with a compliance audit.

| Call | Units | When |
|---|---|---|
| `channels.list` | 1 | once per channel, ever (resolves a handle, finds the uploads playlist; cached in the checkpoint) |
| `playlistItems.list` (uploads playlist, 50 newest) | 1 | every poll, per channel |
| `videos.list` (all recent uploads in one batched call) | 1 | only when a channel has something new |
| `commentThreads.list` | 1 | per new video, if `fetch_comments` |

`search.list` (100/day bucket) and `captions.list` (50 units, returns only
language names) are not used. At the default 1h poll a channel costs about 24
units/day plus about 2 per new video.

New uploads are detected by `contentDetails.videoPublishedAt` against a
per-channel checkpoint. The first poll of a channel stages only its newest
`initial_backfill` uploads. A `quotaExceeded` response ends the poll early.

## Staged item

`source: youtube`, `content_type: text/markdown`, `sender: <channel id>`,
`subject: <video title>`. The body starts with machine-readable lines:

```
# <title>

- video_id: <id>
- watch_url: https://www.youtube.com/watch?v=<id>
- thumbnail_url: <largest available>
- channel: <title> (<@handle>, <UC id>)
- channel_url: https://www.youtube.com/channel/<UC id>
- published_at: <RFC 3339>
- duration: <m:ss or h:mm:ss>
- category_id: <videoCategory id>
- tags: <creator tags>
- topics: <topicDetails names>
- default_audio_language: <lang>
- viewership: high | medium | low | unknown
- comment_activity: high | medium | low | unknown
- engagement_note: <present when measured under 48h after publish>
- transcript: available (<lang>) | unavailable (<reason>) | skipped (<reason>) | error (<reason>)
```

followed by `## Chapters` (each `- <m:ss> <title> (offset_seconds: N)`),
`## Description`, `## Top comments` and, when available, `## Transcript`.

**Chapters** are not an API field. They are parsed from timestamp lines in the
description using YouTube's own rule: the first is 0:00, there are at least
three, and they ascend. Otherwise no chapters are emitted.

**Engagement buckets, not counts.** YouTube API Services Developer Policy
III.E.4 says data read with an API key must be refreshed or deleted within 30
days, and statistics in particular may not be stored longer than 30 days. Raw
view, like and comment counts are therefore never staged. The connector ranks
each video against the channel's other recent uploads (up to 50, same
`videos.list` call) by views per day and comments per day since publish, and
labels the bottom third `low`, the top third `high`, the rest `medium`. With
fewer than 5 comparable videos the label is `unknown`. Buckets are a snapshot
at fetch time; a video fetched within its first hours ranks low because it
has not had time to accumulate views, which `engagement_note` calls out.

The general 30-day clause still applies to the titles, descriptions and
thumbnails held downstream (caro). Refresh/expiry is tracked in glovebox-0tzc.

**Transcripts.** The Data API cannot supply transcripts for videos you do not
own (`captions.download` needs edit permission on the video). The transcript
step is a `TranscriptProvider` seam; the default provider `none` marks every
item `transcript: unavailable (no transcript provider configured)`. Videos in
`transcript.skip_categories` (default Music, `10`) are marked `skipped`.

## Configuration

Sample: [`connectors/youtube/config.json`](../../connectors/youtube/config.json).

Framework fields (from `connector.BaseConfig`, shared by every connector):

| field | type | required | description |
|-------|------|----------|-------------|
| `rules` | `[]Rule` | yes | routing rules; each has `match`, `destination`, optional `tags`, `data_subject`, `audience` |
| `identity` | object | no | `provider` / `auth_method` / `tenant` identity stamped on staged items |
| `fetch_limits` | object | no | `per_source` / `per_poll` caps (0 = unlimited) |
| `data_subject_default` | string | no | default data subject when a rule sets none |
| `audience_default` | `[]string` | no | default audience when a rule sets none |

Connector-specific fields:

| field | type | default | description |
|-------|------|---------|-------------|
| `channels` | `[]string` | (required) | channel IDs (`UC...`) or handles (`@name`) |
| `channel_ids` | `[]string` | | original name for `channels`, still accepted; both are followed |
| `poll_interval` | duration | `1h` | how often every channel is checked; minimum `5m` |
| `initial_backfill` | int | `3` | uploads staged the first time a channel is polled |
| `max_new_per_poll` | int | `10` | cap per channel per poll; the newest are kept |
| `fetch_comments` | bool | `true` | include top comments (by relevance) |
| `max_comments` | int | `25` | comments per video (max 100) |
| `transcript.provider` | string | `none` | transcript provider |
| `transcript.languages` | `[]string` | `["en"]` | preferred transcript languages |
| `transcript.skip_categories` | `[]string` | `["10"]` | category IDs that get no transcript |

`fetch_captions` (removed) is ignored if present.

## Routing

The match key for a channel is `channel:<entry as written in channels>`, for
example `channel:@JeffGeerling` or `channel:UC...`. A `*` rule covers every
channel. The destination must be in the deployment's `agentAllowlist`
(the chart default allows `media`).

## Enabling in the Helm chart

```yaml
connectors:
  youtube:
    enabled: true
    image:
      tag: "<chart version>"
    config:
      rules:
        - match: "*"
          destination: "media"
          tags: { source_type: "video" }
      channels:
        - "@ExampleHandle"
      poll_interval: "1h"
    externalSecret:
      enabled: true
      secretStoreRef: { kind: ClusterSecretStore, name: vault-backend }
      data:
        YOUTUBE_API_KEY:
          key: eso/glovebox-youtube
          property: YOUTUBE_API_KEY
```

## Integration test

No live integration test exists yet; adding
`connectors/youtube/live_integration_test.go` (wiring `YOUTUBE_API_KEY` from
the in-cluster ESO secret, running only under `GLOVEBOX_INTEGRATION=1` and
skipping cleanly elsewhere) is a follow-up.
