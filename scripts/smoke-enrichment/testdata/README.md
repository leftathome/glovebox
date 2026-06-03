# scripts/smoke-enrichment/testdata

Fixtures for the spec-14 §7.3 end-to-end enrichment smoke (driven by
`scripts/smoke-enrichment.sh`).

Each file here is a verbatim copy of the corresponding per-enricher unit-
test fixture; the smoke harness deliberately reuses these so a behavioral
change in any enricher's output shows up first in the unit tests, not in
the slower container smoke.

| File                        | Source of truth                                                | Purpose                                                                  |
|-----------------------------|----------------------------------------------------------------|--------------------------------------------------------------------------|
| `email-body.html`           | `connector/enrich/html/testdata/simple-paragraph.html`         | HTML body of an "email" item; exercises the `html` enricher.             |
| `attachment-report.pdf`     | `connector/enrich/pdf/testdata/text.pdf`                        | Text PDF; exercises the `pdf` enricher (pdftotext).                      |
| `attachment-screenshot.png` | `connector/enrich/ocr/testdata/hello.png`                       | PNG containing rendered text; exercises the `ocr` enricher (tesseract).  |
| `attachment-blank.png`      | `connector/enrich/ocr/testdata/blank.png`                       | PNG with no text; exercises the OCR "image with no text" path (sidecar present, content allowed-empty per spec §7.1). |

When updating any of these fixtures, also update the corresponding file
under `connector/enrich/*/testdata/` and the per-enricher Go test that
consumes it. The smoke harness will refuse to start (exit 2) if any
fixture is missing.
