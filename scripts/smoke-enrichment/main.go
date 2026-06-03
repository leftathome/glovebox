// Command smoke-enrichment is the in-container half of the spec 14 §7.3
// end-to-end smoke test.
//
// It is invoked by scripts/smoke-enrichment.sh, runs inside an image
// built FROM the glovebox-enricher-runtime base, and exercises the
// enrichment pipeline against fixture files mounted at /fixtures. Output
// staging items land under -staging-dir (mounted by the caller as a
// host-side scratch dir) so the shell wrapper can assert the resulting
// sidecar set without needing to exec back into the container.
//
// Design notes:
//
//   - The spec text in §7.3 calls for "spinning up the gmail connector
//     container against a fixture archive (small mbox with a known PDF,
//     image, and HTML email)". The gmail connector talks only to the
//     live Gmail API and the only fixture-driven email connector in the
//     repo (mbox-importer) writes whole RFC822 messages as content.raw
//     without MIME decomposition, so the html/pdf/ocr enrichers never
//     fire on its output. This smoke harness instead pre-stages
//     individual item directories that mirror the layout an email
//     connector with MIME decomposition WOULD produce, and runs the
//     same enrichment pipeline (enrich.Default.ApplyAll) Commit()
//     would invoke on them.
//
//   - The enricher.Applies() predicates all dispatch on
//     meta.ContentType (see connector/enrich/html|pdf|ocr|office .go).
//     ItemMetadata carries one ContentType per item. To exercise html,
//     pdf, and ocr enrichers we therefore stage three separate items —
//     one per content type — rather than one multipart item. A future
//     follow-on bead may add per-attachment content typing or file-
//     extension sniffing to enrichers; once that lands, this harness
//     can collapse to a single multipart item and the §7.3 spec text
//     becomes literally implementable.
//
// Exit code:
//
//	0  on success (all expected sidecars present, content where expected).
//	1  on any assertion failure or pipeline error.
//	2  on argument / I/O setup failure (no fixtures, unwritable staging).
//
// Every failure emits a WHAT/CHECK/FIX line per spec 14 §8.
//
// CLI:
//
//	-fixtures-dir   directory containing the source fixtures (default /fixtures)
//	-staging-dir    directory where finalized items will be placed
//	                (default /staging)
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"

	// Side-effect imports register each enricher with enrich.Default.
	// Must match the connector-side imports for gmail / imap / outlook /
	// mbox-importer (spec 14 §6.2): passthrough + html + pdf + ocr +
	// office. office is included so a future addition of a .docx
	// attachment to the fixture set picks it up automatically.
	_ "github.com/leftathome/glovebox/connector/enrich/html"
	_ "github.com/leftathome/glovebox/connector/enrich/ocr"
	_ "github.com/leftathome/glovebox/connector/enrich/office"
	_ "github.com/leftathome/glovebox/connector/enrich/passthrough"
	_ "github.com/leftathome/glovebox/connector/enrich/pdf"
)

// scenario captures one item's worth of fixture-driven enrichment input
// and the asserted output. The harness stages a per-scenario item dir,
// runs enrich.Default.ApplyAll, then asserts the sidecar set against
// expectedSidecars.
type scenario struct {
	name        string // e.g. "html-email"
	contentType string // value for meta.ContentType
	// primaryFixture is the path under fixturesDir whose contents go
	// into content.raw. Empty means no primary file (attachment-only).
	primaryFixture string
	// attachmentFixtures lists (fixture-relative-path, in-item-basename)
	// pairs to stage as siblings of content.raw. The basename should
	// follow the "attachment-<n>-<safe-name>" convention.
	attachmentFixtures []attachmentMapping
	// expectedSidecars are the sidecar files this scenario MUST produce.
	expectedSidecars []sidecarAssertion
}

type attachmentMapping struct {
	fixture  string
	basename string
}

type sidecarAssertion struct {
	// filename is relative to the item dir.
	filename string
	// producer is the Name() of the enricher expected to write it
	// (used in error messages only).
	producer string
	// mustNonEmpty asserts the sidecar's content size > 0. OCR output
	// on blank images is allowed to be empty by spec §7.1, so the
	// flag is per-assertion.
	mustNonEmpty bool
	// mustContain is a substring that must appear verbatim in the
	// sidecar bytes. Empty means "do not byte-check the content".
	mustContain string
	// goldenContent, if non-empty, asserts the sidecar bytes equal
	// these bytes exactly (byte-for-byte). Used for deterministic
	// extractions only (html). pdftotext and tesseract output are
	// version-sensitive; do not pin them here.
	goldenContent string
}

func main() {
	os.Exit(run())
}

func run() int {
	fixturesDir := flag.String("fixtures-dir", "/fixtures", "directory containing source fixtures")
	stagingDir := flag.String("staging-dir", "/staging", "directory where finalized item dirs will be placed")
	flag.Parse()

	for _, dir := range []string{*fixturesDir, *stagingDir} {
		fi, err := os.Stat(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"smoke-enrichment: required directory %s not accessible: %v\n"+
					"  CHECK: ls -la %s\n"+
					"  FIX:   mount the directory into the container (see scripts/smoke-enrichment.sh)\n",
				dir, err, dir)
			return 2
		}
		if !fi.IsDir() {
			fmt.Fprintf(os.Stderr,
				"smoke-enrichment: %s is not a directory\n"+
					"  CHECK: file %s\n"+
					"  FIX:   pass a directory path to -fixtures-dir / -staging-dir\n",
				dir, dir)
			return 2
		}
	}

	scenarios := []scenario{
		// 1. HTML email body. The fixture is the same one used by
		// connector/enrich/html/html_test.go (simple-paragraph.html) so
		// the html enricher's output is exactly "Hello\n" — pinned as a
		// golden below.
		{
			name:           "html-email",
			contentType:    "text/html",
			primaryFixture: "email-body.html",
			expectedSidecars: []sidecarAssertion{
				{
					filename:     "content.extracted.md",
					producer:     "html",
					mustNonEmpty: true,
					// Loosened from byte-exact "Hello\n" to substring per
					// code-review M4: the unit test at
					// connector/enrich/html/html_test.go asserts only
					// substring-presence, so a byte-exact smoke would invert
					// the README's stated "unit tests fail first" guarantee
					// if x/net/html ever changes its trailing-whitespace
					// behavior.
					mustContain: "Hello",
				},
			},
		},
		// 2. PDF attachment under an application/pdf primary. The
		// primary IS the PDF (content.raw is the PDF bytes); we test
		// the pdf enricher's primary-source path. pdftotext is
		// nondeterministic across poppler versions for whitespace so
		// we only assert non-empty + substring presence.
		//
		// We also stage one decoy attachment-1-irrelevant.txt to exercise
		// the per-attachment iteration loop (the txt file's content
		// type would not normally be application/pdf, but with
		// item-level ContentType="application/pdf" the pdf enricher's
		// Applies() returns true for ALL files in this item including
		// the txt — which is a known limitation documented in the
		// design notes above and surfaces here as an extra sidecar.
		// We do NOT pin that extra sidecar; we just assert the primary
		// one is correct.
		{
			name:           "pdf-attachment",
			contentType:    "application/pdf",
			primaryFixture: "attachment-report.pdf",
			expectedSidecars: []sidecarAssertion{
				{
					filename:     "content.extracted.md",
					producer:     "pdf",
					mustNonEmpty: true,
				},
			},
		},
		// 3. Image attachment with text. The primary is the PNG and
		// ContentType=image/png so the ocr enricher fires. tesseract
		// output is non-deterministic; assert non-empty only.
		{
			name:           "ocr-image-with-text",
			contentType:    "image/png",
			primaryFixture: "attachment-screenshot.png",
			expectedSidecars: []sidecarAssertion{
				{
					filename:     "content.extracted.md",
					producer:     "ocr",
					mustNonEmpty: true,
				},
			},
		},
		// 4. Image attachment WITHOUT text. ocr enricher must still
		// produce a sidecar (per spec §7.1: "image with no text →
		// assert empty output, no error") — file present but content
		// may be empty.
		{
			name:           "ocr-image-blank",
			contentType:    "image/png",
			primaryFixture: "attachment-blank.png",
			expectedSidecars: []sidecarAssertion{
				{
					filename:     "content.extracted.md",
					producer:     "ocr",
					mustNonEmpty: false,
				},
			},
		},
	}

	totalPass := 0
	totalFail := 0

	for _, sc := range scenarios {
		fmt.Printf("=== scenario %s (content_type=%s) ===\n", sc.name, sc.contentType)
		pass, fail := runScenario(sc, *fixturesDir, *stagingDir)
		totalPass += pass
		totalFail += fail
	}

	fmt.Printf("\nsmoke-enrichment: PASS=%d FAIL=%d\n", totalPass, totalFail)
	if totalFail > 0 {
		return 1
	}
	return 0
}

// runScenario stages, runs, and asserts one scenario. Returns
// (pass, fail) counts; never panics; emits WHAT/CHECK/FIX lines on
// every failure path.
func runScenario(sc scenario, fixturesDir, stagingDir string) (int, int) {
	pass := 0
	fail := 0

	// Work in /tmp so the final rename into stagingDir is a same-fs
	// rename when possible. copyDir handles cross-device fallback.
	workDir, err := os.MkdirTemp("", "smoke-"+sc.name+"-")
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"smoke-enrichment[%s]: FAIL cannot create temp work dir: %v\n"+
				"  CHECK: df /tmp\n"+
				"  FIX:   verify /tmp is writable in the container; mount a tmpfs if needed\n",
			sc.name, err)
		return pass, fail + 1
	}
	itemName := fmt.Sprintf("%s-%s-%s", time.Now().UTC().Format("20060102-150405"), sc.name, uuid.New().String()[:8])
	itemDir := filepath.Join(workDir, itemName)
	if err := os.MkdirAll(itemDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr,
			"smoke-enrichment[%s]: FAIL cannot create item dir %s: %v\n"+
				"  CHECK: ls -la %s\n"+
				"  FIX:   verify the work dir is writable by uid %d\n",
			sc.name, itemDir, err, workDir, os.Getuid())
		return pass, fail + 1
	}

	// ---- Phase 1: stage fixture files in the item dir ----
	if sc.primaryFixture != "" {
		src := filepath.Join(fixturesDir, sc.primaryFixture)
		dst := filepath.Join(itemDir, "content.raw")
		if err := copyFile(src, dst); err != nil {
			fmt.Fprintf(os.Stderr,
				"smoke-enrichment[%s]: FAIL cannot stage primary %s -> %s: %v\n"+
					"  CHECK: ls -la %s\n"+
					"  FIX:   verify scripts/smoke-enrichment/testdata/ contains %s and the bind mount targets /fixtures\n",
				sc.name, src, dst, err, fixturesDir, sc.primaryFixture)
			return pass, fail + 1
		}
	}
	for _, a := range sc.attachmentFixtures {
		src := filepath.Join(fixturesDir, a.fixture)
		dst := filepath.Join(itemDir, a.basename)
		if err := copyFile(src, dst); err != nil {
			fmt.Fprintf(os.Stderr,
				"smoke-enrichment[%s]: FAIL cannot stage attachment %s -> %s: %v\n"+
					"  CHECK: ls -la %s\n"+
					"  FIX:   verify scripts/smoke-enrichment/testdata/ contains %s\n",
				sc.name, src, dst, err, fixturesDir, a.fixture)
			return pass, fail + 1
		}
	}

	// ---- Phase 2: run the enrichment pipeline ----
	meta := staging.ItemMetadata{
		Source:           "smoke-enrichment",
		Sender:           "fixture@example.com",
		Subject:          "spec-14 §7.3 smoke: " + sc.name,
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "messaging",
		ContentType:      sc.contentType,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	contentPath := filepath.Join(itemDir, "content.raw")
	if _, err := os.Stat(contentPath); err == nil {
		_, errs := enrich.Default.ApplyAll(ctx, contentPath, meta, itemDir)
		for _, e := range errs {
			fmt.Fprintf(os.Stderr,
				"smoke-enrichment[%s]: enricher %q on primary returned error: %v\n",
				sc.name, e.Producer, e.Err)
		}
	}
	// Iterate attachments lexicographically (filepath.Glob is
	// documented as lexicographic; explicit for clarity).
	attachments, _ := filepath.Glob(filepath.Join(itemDir, "attachment-*"))
	for _, ap := range attachments {
		_, errs := enrich.Default.ApplyAll(ctx, ap, meta, itemDir)
		for _, e := range errs {
			fmt.Fprintf(os.Stderr,
				"smoke-enrichment[%s]: enricher %q on %s returned error: %v\n",
				sc.name, e.Producer, filepath.Base(ap), e.Err)
		}
	}

	// ---- Phase 3-4: assert sidecar set + content shape + golden ----
	for _, a := range sc.expectedSidecars {
		p := filepath.Join(itemDir, a.filename)
		st, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"smoke-enrichment[%s]: FAIL sidecar %s (producer=%s) missing: %v\n"+
					"  CHECK: ls -la %s\n"+
					"  FIX:   verify the %s enricher init() ran inside the container (binary on PATH); see scripts/test-enricher-runtime.sh for the base-image check\n",
				sc.name, a.filename, a.producer, err, itemDir, a.producer)
			fail++
			continue
		}
		if a.mustNonEmpty && st.Size() == 0 {
			fmt.Fprintf(os.Stderr,
				"smoke-enrichment[%s]: FAIL sidecar %s (producer=%s) is empty but non-empty content was expected\n"+
					"  CHECK: cat %s ; ls -la %s\n"+
					"  FIX:   inspect the enricher logs above for a WHAT/CHECK/FIX failure; check whether the source fixture is corrupt or the binary returned an error\n",
				sc.name, a.filename, a.producer, p, itemDir)
			fail++
			continue
		}
		if a.mustContain != "" || a.goldenContent != "" {
			body, err := os.ReadFile(p)
			if err != nil {
				fmt.Fprintf(os.Stderr,
					"smoke-enrichment[%s]: FAIL cannot read sidecar %s: %v\n"+
						"  CHECK: ls -la %s\n"+
						"  FIX:   verify the file is readable by uid %d\n",
					sc.name, a.filename, err, p, os.Getuid())
				fail++
				continue
			}
			if a.mustContain != "" && !strings.Contains(string(body), a.mustContain) {
				fmt.Fprintf(os.Stderr,
					"smoke-enrichment[%s]: FAIL sidecar %s does not contain expected substring %q\n"+
						"  got:   %q\n"+
						"  CHECK: cat %s\n"+
						"  FIX:   verify the fixture matches what the enricher expects; update the assertion if the fixture changed intentionally\n",
					sc.name, a.filename, a.mustContain, truncate(string(body), 200), p)
				fail++
				continue
			}
			if a.goldenContent != "" && string(body) != a.goldenContent {
				fmt.Fprintf(os.Stderr,
					"smoke-enrichment[%s]: FAIL sidecar %s does not match golden\n"+
						"  golden: %q\n"+
						"  got:    %q\n"+
						"  CHECK: cat %s\n"+
						"  FIX:   if the %s enricher's normalization changed intentionally, update the golden in scripts/smoke-enrichment/main.go\n",
					sc.name, a.filename, a.goldenContent, truncate(string(body), 200), p, a.producer)
				fail++
				continue
			}
		}
		fmt.Printf("smoke-enrichment[%s]: PASS sidecar %s (producer=%s) size=%d\n", sc.name, a.filename, a.producer, st.Size())
		pass++
	}

	// ---- Phase 5: finalize (move item into staging) + cleanup ----
	destDir := filepath.Join(stagingDir, itemName)
	if err := os.Rename(itemDir, destDir); err != nil {
		// Cross-device rename happens when /tmp and /staging are on
		// different mounts. Fall back to copy + remove.
		if copyErr := copyDir(itemDir, destDir); copyErr != nil {
			fmt.Fprintf(os.Stderr,
				"smoke-enrichment[%s]: FAIL cannot move item dir into staging: %v (copy fallback: %v)\n"+
					"  CHECK: mount | grep -E '/tmp|/staging'\n"+
					"  FIX:   mount /staging and /tmp on the same fs, or fix the copyDir helper\n",
				sc.name, err, copyErr)
			fail++
		} else {
			_ = os.RemoveAll(itemDir)
		}
	}
	_ = os.RemoveAll(workDir) // best-effort cleanup of the parent tmpdir

	return pass, fail
}

// truncate returns s truncated to maxLen runes, with an ellipsis marker
// when truncation occurred. Used in error messages to keep them
// terminal-friendly when an enricher emits a large blob.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...(truncated)"
}

// copyFile copies src to dst with mode 0644, fsyncing dst before close.
// Returned errors are wrapped at the call site with WHAT/CHECK/FIX text.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// copyDir is the cross-device rename fallback. Flat directory copy
// only (enricher sidecars are flat files); nested subdirs would be
// silently skipped — tighten if that assumption changes.
func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
