# glovebox <-> nagus connector integration

- **Status:** design record (2026-07-02)
- **Bead:** glovebox-t6fz
- **Canonical design:** the full acquisition/watch subsystem design lives in the
  **agentic/nagus** repo at `docs/design/2026-07-01-nagus-design.md`. This doc records
  only glovebox's slice.

> **Scope update (2026-08-22): items 1 and 2 below were dropped and never built.**
> On 2026-07-02 -- the same bead (glovebox-t6fz), the same day this record was
> written -- the operator chose **"glovebox = sanitizer boundary only"** over
> "glovebox owns the connectors" (see
> [`docs/superpowers/specs/2026-07-02-sanitize-gate-design.md`](../superpowers/specs/2026-07-02-sanitize-gate-design.md)
> section 2). nagus already had its own code-complete eBay Browse connector and a
> Craigslist connector, so building a second pair here would have duplicated
> nagus's code without moving the injection boundary anywhere new. glovebox
> instead shipped the synchronous **`POST /v1/sanitize`** gate that nagus calls
> from its own connectors -- same boundary, no duplicated fetch code. See
> [`docs/sanitize-gate.md`](../sanitize-gate.md).
>
> As of 2026-08-22 there is no `cmd/connector-ebay` (`cmd/` holds only
> `corpus-gate` and `rules-sign`) and no Craigslist feed or routing rule in any
> shipped RSS config. The eBay ToS and Craigslist egress caveats recorded below
> stand as written; they are part of *why* the split moved, and they still apply
> to whoever owns those fetchers. The sections are kept intact rather than deleted
> so the original reasoning stays readable.

## Why glovebox is involved

nagus is a multi-category acquisition/watch subsystem (monitor -> sanitize -> extract
-> normalize -> store -> enrich -> score -> surface). Per the glovebox boundary rule
(**content type, not source type**: any channel where a human can put arbitrary natural
language must cross the sanitization boundary), nagus does **not** fetch untrusted
listing text itself. glovebox fetches + sanitizes the free-text listing sources and
hands off normalized items; nagus fetches the structured reference/enrichment APIs
directly.

## Split for the v1 categories (land, HDD)

| Source | Free-text? | Owner |
|---|---|---|
| **eBay Browse** (HDD used/refurb listings; later durables broadly) | Yes (title/description) | ~~glovebox -- NEW `cmd/connector-ebay`~~ **DROPPED 2026-07-02 -- nagus-side connector calls `POST /v1/sanitize`** |
| **Craigslist `reo`** (land listings) | Yes (body) | ~~glovebox -- existing `connector-rss` + config~~ **DROPPED 2026-07-02 -- nagus-side connector calls `POST /v1/sanitize`** |
| Zillapi/Rentcast *listing description* (land) | Yes (small blurb) | route that field through sanitization (small connector or sanitize hop; decide when land is wired) |
| ServerPartDeals `products.json`, diskprices $/TB | No (retailer catalog / numeric table) | **nagus-direct** (structured, no injection surface) |
| Gov geo (FEMA/USGS/USDA/USFWS/Census), parcel data | No (typed API schemas) | **nagus-direct** enrichment |

## Work items (glovebox-t6fz)

1. ~~**`cmd/connector-ebay`** (new)~~ **-- DROPPED 2026-07-02, not built.** eBay Browse API (OAuth client-credentials, ~5k/day).
   - CAVEAT: eBay's Feb-2026 ToS tightened against AI agents, and full production Browse
     data is EPN-partner-gated (Application Growth Check) -- validate a real keyset's data
     completeness before committing to eBay as the durables spine.
   - Sanitize `title`/`description` free-text; pass structured fields typed (price,
     `conditionId` enum 1000 New / 2000 Certified-Refurb / 2500 Seller-Refurb / 3000 Used
     / 7000 Parts, item specifics). Emit a normalized item.
2. ~~**Craigslist via `connector-rss`** (config, not new code)~~ **-- DROPPED 2026-07-02, not configured.** Add the `reo` search feed
   (`https://<region>.craigslist.org/search/reo?format=rss`) + a routing rule tagging the
   items for nagus.
   - OPS CAVEAT: Craigslist RSS 403s datacenter/cluster egress IPs -- needs a
     residential-routed poller/proxy (a gitops/networking task).
3. **Handoff contract (glovebox -> nagus):** *(superseded 2026-07-02 -- the handoff is now the synchronous `POST /v1/sanitize` request/response, not a shared staging area or a push to a nagus ingest endpoint.)* define where the **extract/tokenize** step
   runs (glovebox side, per the nagus design) and how sanitized normalized items reach
   nagus's store -- either a shared staging area (the existing `inbox/<id>/{content.raw,
   content.extracted.md, metadata.json}` pattern) that nagus consumes, or a push to a
   nagus ingest endpoint. The connector registry must authorize the nagus destination and
   a per-connector staging subdir.

## Injection containment

The extract step emits a **constrained typed schema**. Even with an LLM fallback on the
sanitized text, output is typed labels (category, flag, number), never free execution --
so a malicious listing can at worst produce a wrong field value, not hijack anything
downstream. Free-text that survives (e.g. a description) is carried as quoted data.
