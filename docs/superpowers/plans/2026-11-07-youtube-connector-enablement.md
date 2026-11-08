# YouTube connector enablement (2026-11-07)

Operator goal: follow a handful of YouTube channels on orac and land one
triage-ready item per new video in caro (feed tier), carrying the metadata that
matters more than the video itself.

Prior design: `gitops/docs/superpowers/specs/2026-08-19-youtube-channel-ingestion-design.md`.
This plan supersedes its discovery section (Atom feed) -- see "Changes since".

## Decisions (operator, 2026-11-07)

- Poll interval: global `poll_interval`, default `1h`. Per-channel override is
  future work (glovebox-i46m).
- Channels for the trial: @Cruisewith, @JeffGeerling, @feedmeimei,
  @moremeimei, @JKenjiLopezAlt. Destination on orac: `main`.
- Per video: thumbnail, description, chapter offsets + titles, watch URL,
  labels, English captions as a transcript.
- Raw view/like/comment counts are NOT staged. Stage relative buckets
  (high/medium/low) computed at fetch time; refresh via the connector if an
  item becomes interesting (glovebox-8wu1, glovebox-0tzc).
- Transcripts: yt-dlp subs-only (`--skip-download`) green-lit, as a follow-up
  provider behind the TranscriptProvider seam (glovebox-9zmo). This MR ships
  the seam with a no-op default and an explicit availability marker.
- Agents adding channels at runtime: designed separately (glovebox-365v).

## Changes since the 2026-08-19 design

- The channel Atom feed is NOT used: 1 success in 12 tries across 4 channels on
  2026-11-07 (intermittent 404s). Discovery is `playlistItems.list` on the
  uploads playlist (1 unit, 50 results, not in the 100/day search bucket).
- Quota at 1h: ~24 units/channel/day + ~2 per new video.
- Developer Policy III.E.4: API-key data must be refreshed or deleted within 30
  days; statistics may not be stored past 30 days. Buckets address the
  statistics sentence; the general 30-day clause still applies to titles,
  descriptions and thumbnails held in caro (glovebox-0tzc).

## Tasks

### openclaw (gate: openclaw-iw1s)
- [x] Port connector-declared tier onto internal/sourceclass (MR !54)
- [ ] Merge, publish image, bump gateway tag in gitops, verify `triage --help`

### glovebox MR 1 (this branch, feat/youtube-enable)
- [x] fy4e: channels.list resolve (handle or UC id, cached) + playlistItems.list discovery; configurable poll_interval (default 1h); first-run backfill cap
- [x] tbgd: drop captions.list
- [x] 07il: default destination `media`
- [x] rrtu: one batched videos.list (snippet,contentDetails,statistics,topicDetails); thumbnail, duration, tags, topics, chapters parsed from description, watch URL; markdown content
- [x] 8wu1: relative engagement buckets; no raw counts staged (regression test)
- [x] iwqm: TranscriptProvider seam, no-op default, explicit marker; music category (10) skipped
- [x] docs: youtube.md, integration-credentials.md (Vault eso/glovebox-youtube / YOUTUBE_API_KEY)
- [x] go test / vet / staticcheck; container build

### release + deploy
- [ ] glovebox release MR (CHANGELOG, Chart.yaml), tag on origin + github
- [ ] gitops: ExternalSecret + connectors.youtube block in orac glovebox.yaml (gitops-0zod)
- [ ] Verify on orac: items staged, diverted to caro, quota usage sane

### glovebox MR 2
- [ ] 9zmo: yt-dlp subs-only transcript provider
