# Adversarial corpus

**The files under `malicious/` are prompt-injection samples. They are inert
text fixtures — nothing here is real content, no address in them resolves, and
nothing executes. Do not "fix" them.** Every homoglyph, invisible character,
reversed string and mangled encoding in this directory is deliberate: it *is*
the test. An editor that helpfully normalises Unicode, strips zero-width
characters, converts line endings or reflows a long line silently destroys the
case it touches, and the corpus goes quietly green while measuring less.

## What this is

A checked-in red-team set for the scanner, plus the benign counterweight that
keeps it honest. Each case is one small file (`malicious/` or `benign/`) with an
entry in `manifest.json` giving the content type, the channel metadata it
arrives with, and the verdict the engine is expected to reach.

The efficacy fixes — homoglyph folding, invisible/Tags-block stripping,
decode-then-scan, whole-document detectors, metadata scanning — each landed with
a targeted regression test. That proves one bypass is closed. It does not
measure how much of a red-team set the scanner catches, and it does not bound
what legitimate mail the scanner destroys on the way. Both numbers come from
here.

## Running it

```
go run ./cmd/corpus-gate            # metrics + exit non-zero on a threshold breach
go run ./cmd/corpus-gate -v         # every case, one line each
go test ./internal/corpus/ -v       # the same run, as part of the test suite
```

`scripts/corpus-gate.sh` is the CI entry point (`test` job in
`.github/workflows/ci.yml`).

The scanner is constructed the way production constructs it: rules loaded from
`configs/default-rules.json`, the default detector registry, `scan.New`, and
`ScanWithMetadata` called with the same `[subject, sender, source]` triple the
worker pool passes. A regression in the shipped rules file fails this gate.

## Coverage

| Directory | Class | Cases | What it exercises |
|-----------|-------|-------|-------------------|
| `malicious/` | `homoglyph` | 5 | Cyrillic and Greek confusables, including a two-character swap and one behind the HTML strip |
| `malicious/` | `invisible` | 9 | Unicode Tags block (U+E0000–U+E007F), zero-width, soft hyphen, word joiner, Mongolian vowel separator, bidi controls |
| `malicious/` | `encoded` | 10 | base64 std/raw/url, short sub-threshold runs, hex, percent (full and partial), `+`-as-space form encoding, nested base64, base64 inside HTML |
| `malicious/` | `mid-document` | 4 | ~140 KiB items with the payload at the midpoint, past any first-64K/last-64K sample window |
| `malicious/` | `metadata` | 6 | Injection in Subject or sender display name, with a benign or empty body |
| `malicious/` | `plain` | 5 | No obfuscation at all — instruction override, role reassignment, tool syntax, HTML comment |
| `benign/` | `benign-ordinary` | 8 | Everyday mail, HTML newsletters, a git patch, a 140 KiB legitimate report |
| `benign/` | `benign-foreign` | 4 | French, German, Spanish, Japanese — the language detector is a ×1.5 booster |
| `benign/` | `benign-encoded` | 3 | An inline base64 image, a PGP signature block, URL tracking parameters |
| `benign/` | `benign-alarming` | 5 | Content that *looks* like an attack and is not: a security advisory quoting an injection, a code review about injection detection, release notes with a shell fence, a support reply about passwords, docs using "act as" in its ordinary sense |
| `benign/` | `benign-invisible-lookalike` | 1 | An emoji ZWJ sequence — legitimate zero-width use |

The benign set is deliberately skewed hard: roughly two fifths of it is content
chosen to be difficult, which no real mailbox looks like. The false-positive
rate below is therefore an upper bound on adversarially-hard legitimate content,
not an estimate of what a production inbox would see. Read it that way, and do
not "improve" it by adding easy passes — padding the denominator with content
the scanner was never going to flag makes the number prettier and the gate
weaker.

## Known gaps

Cases the engine gets wrong today carry `"known_gap": true` in `manifest.json`
with a `gap_note` explaining the mechanism. **They stay in the corpus and stay
counted in the rates.** Deleting a case the scanner misses, or weakening it
until it passes, converts a measurement into a decoration. The per-case
assertion inverts for a known gap, so if a fix lands and the case starts
behaving, the runner says `NO LONGER FAILING` and the test fails until the flag
is cleared here — a gap cannot silently close any more than it can silently
open.

As of 2026-08-22: **no missed malicious cases** and **5 false positives**
(`base64-image-attachment`, `pgp-signature`,
`security-advisory-quoting-injection`, `release-notes-with-shell`,
`docs-act-as-proxy`). See each `gap_note` for why.

All three detection gaps recorded here are closed. `encoded-percent-partial`
and `invisible-bidi-controls` went first: the scanner unescapes
percent/HTML-entity/backslash escapes in place and applies the UAX #9
explicit-embedding rules to build a bidi-reordered scan view.
`encoded-plus-form` followed: `+` is decoded to a space inside a URL query
component — and inside a query lifted out of its URL — but nowhere else, so
`C++`, `A+` and `+1 555 0100` keep theirs. Detection is 39/39, so
`min_detection_rate` is 1.0 and every malicious case in this directory is
load-bearing.

## Adding a case

1. Add the fixture under `malicious/` or `benign/`.
2. Add a `manifest.json` entry: `id` (unique), `file`, `content_type`,
   `subject` / `sender` / `source`, `expect` (`quarantine` or `pass`), `class`,
   and a `note` saying what the case is *for* — the class of evasion, not the
   text.
3. Run `go run ./cmd/corpus-gate`. If the engine gets it wrong, mark it
   `known_gap` with a `gap_note` and report it. Do not adjust the fixture until
   it passes.
4. Adding a case moves the rates, so `thresholds.json` needs updating with the
   newly measured numbers. Commit the measured values.
