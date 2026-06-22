// media_services.go -- normalization of the Apple media-services bucket's
// purchase data (glovebox-ot7v, priority acceptance openclaw-e6f).
//
// The skeleton importer (importer.go) passthrough-stages every tree/ file as-is.
// That keeps the raw export but is not queryable: OpenClaw cannot answer "what
// did I buy on the iTunes Store" from a 600KB CSV (or one buried inside a nested
// zip). This file adds an ADDITIVE pass for the apple/media-services bucket: it
// locates the iTunes/App Store purchase-history CSV(s) -- which may live at the
// tree/ top level AND inside the nested Apple_Media_Services.zip -- normalizes
// each into a Purchase list, and stages ONE LLM-readable markdown item per
// purchase file (Content-Type text/markdown) summarizing the purchases. The raw
// passthrough items are still produced; this pass only adds the normalized view.
//
// Schema discovery is HEADER-DRIVEN and tolerant. Real Apple files vary:
//   - "Store Transaction Purchase and Free Apps History.csv" (in the nested zip)
//     keys purchases on: Item Description, Item Purchased Date, Content Type,
//     Invoice Item Total, Currency, Order Number, Item Reference Number, ...
//   - "iTunes and App-Book Re-download and Update History.csv" (tree top level)
//     keys on: Item Description, Activity Date, Content Type, Item Reference
//     Number, Seller, ...
//
// Columns are matched by name with fallbacks (never by fixed position); missing
// columns are left empty so an evolving export degrades gracefully.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/leftathome/glovebox/internal/ingest/archives"
)

// mediaServicesBucket is the matcher_id that selects the purchase-normalization
// pass. Deliveries with this matcher_id get the additive markdown items.
const mediaServicesBucket = "apple/media-services"

// importReferenceName is the basename of the apple-import-reference.json
// deliverable (parsed for per-bucket data_files hints when present in tree/).
const importReferenceName = "apple-import-reference.json"

// Purchase is one normalized iTunes / App Store purchase row. Every field is a
// trimmed string sourced from a header-matched column (empty when the source
// file lacks that column). Title is required; rows without one are skipped.
type Purchase struct {
	Title    string // Item Description
	Date     string // Item Purchased Date / Activity Date / Purchase Created Date
	Type     string // Content Type (e.g. "Mac Apps", "iOS and tvOS Apps", "Music")
	Cost     string // Invoice Item Total / Invoice Total Amount / Invoice Subtotal Amount
	Currency string // Currency
	Order    string // Order Number / Document Number
	ItemRef  string // Item Reference Number
}

// Column fallback lists (lowercased). The first non-empty match wins. Order
// matters: most specific real header first, generic fallbacks last.
var (
	colTitle    = []string{"item description", "title", "item name"}
	colDate     = []string{"item purchased date", "purchase date", "activity date", "purchase created date", "invoice date", "date"}
	colType     = []string{"content type", "media type", "type"}
	colCost     = []string{"invoice item total", "invoice total amount", "invoice subtotal amount", "amount", "price", "cost"}
	colCurrency = []string{"currency"}
	colOrder    = []string{"order number", "document number"}
	colItemRef  = []string{"item reference number", "item reference", "item id"}
)

// purchaseNamePatterns are lowercased basename substrings that identify a known
// purchase-history CSV when no import-reference hint is available.
var purchaseNamePatterns = []string{
	"store transaction purchase",
	"purchase and free apps",
	"re-download and update history",
	"redownload and update history",
	"app store purchase history",
}

// purchaseExcludePatterns are lowercased basename substrings for high-volume
// event logs that contain "purchase" but are NOT purchase records.
var purchaseExcludePatterns = []string{
	"flow events",
	"server events",
}

// csvCandidate is one CSV located during the tree scan. RelName is a logical
// source path used for provenance and the markdown's source-file line (nested
// entries use "<zip>!/<inner path>"); AbsPath is where the bytes live on disk
// (the tree file itself, or an extracted temp file for nested entries).
type csvCandidate struct {
	RelName string
	AbsPath string
}

// stageMediaServicesPurchases locates the purchase-history CSV(s) under
// treeRoot (top-level files plus any extracted from nested zips), normalizes
// each, and stages one markdown item per file with a non-empty purchase list.
// It returns the number of normalized items committed. The raw passthrough
// items committed by Import are unaffected; this pass is purely additive.
func (a *appleImporter) stageMediaServicesPurchases(
	treeRoot string,
	receipt *archives.FinalizeReceipt,
) (int, error) {
	hints := loadPurchaseHints(treeRoot)

	candidates, cleanup, err := collectMediaServicesCSVs(treeRoot)
	defer cleanup()
	if err != nil {
		return 0, fmt.Errorf("collect media-services csvs: %w", err)
	}

	// Deterministic order so repeated imports stage items consistently.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].RelName < candidates[j].RelName
	})

	staged := 0
	for _, c := range candidates {
		if !isPurchaseFile(c.RelName, hints) {
			continue
		}
		purchases, err := readPurchases(c.AbsPath)
		if err != nil {
			return staged, fmt.Errorf("read purchases %s: %w", c.RelName, err)
		}
		if len(purchases) == 0 {
			// A hinted/pattern file with no titled rows is not a purchase
			// table (e.g. a billing or account-history CSV); skip it.
			continue
		}

		md := renderPurchasesMarkdown(c.RelName, purchases)

		// Route + provenance through the normal BuildItemOptions path so the
		// configured data_subject and apple_bucket tag are applied. A
		// synthetic "media-services/<name>.md" RelPath yields a stable rule key
		// (apple:media-services); the real source path is recorded separately.
		base := filepath.Base(c.RelName)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		entry := appleEntry{
			RelPath:     "media-services/" + stem + ".md",
			ContentType: "text/markdown",
		}
		opts, err := BuildItemOptions(entry, receipt, a.fw.Matcher, a.sourceName, a.fixedTags)
		if err != nil {
			return staged, fmt.Errorf("build_item_options %s: %w", c.RelName, err)
		}
		opts.Tags["apple_source_file"] = c.RelName

		item, err := a.fw.Backend.NewItem(opts)
		if err != nil {
			return staged, fmt.Errorf("backend_new_item %s: %w", c.RelName, err)
		}
		if err := item.WriteContent([]byte(md)); err != nil {
			return staged, fmt.Errorf("write_content %s: %w", c.RelName, err)
		}
		if err := item.Commit(); err != nil {
			return staged, fmt.Errorf("commit %s: %w", c.RelName, err)
		}
		staged++
	}

	return staged, nil
}

// collectMediaServicesCSVs walks treeRoot and returns every CSV candidate: the
// .csv files directly under tree/, plus the .csv files found by extracting each
// top-level .zip (e.g. Apple_Media_Services.zip) into a temp dir. The returned
// cleanup function removes those temp dirs and must always be called. Nested
// zips inside zips are not recursed (the purchase files sit one level deep).
func collectMediaServicesCSVs(treeRoot string) ([]csvCandidate, func(), error) {
	var (
		candidates []csvCandidate
		tmpDirs    []string
	)
	cleanup := func() {
		for _, d := range tmpDirs {
			_ = os.RemoveAll(d)
		}
	}

	walkErr := filepath.WalkDir(treeRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(treeRoot, p)
		if relErr != nil {
			return fmt.Errorf("relativize %s: %w", p, relErr)
		}
		rel = filepath.ToSlash(rel)

		switch strings.ToLower(filepath.Ext(p)) {
		case ".csv":
			candidates = append(candidates, csvCandidate{RelName: rel, AbsPath: p})
		case ".zip":
			tmp, mkErr := os.MkdirTemp("", "apple-ms-")
			if mkErr != nil {
				return fmt.Errorf("mkdtemp for %s: %w", rel, mkErr)
			}
			tmpDirs = append(tmpDirs, tmp)
			written, upErr := UnpackZip(p, tmp)
			if upErr != nil {
				return fmt.Errorf("unpack nested zip %s: %w", rel, upErr)
			}
			for _, w := range written {
				if strings.EqualFold(filepath.Ext(w), ".csv") {
					candidates = append(candidates, csvCandidate{
						RelName: rel + "!/" + w,
						AbsPath: filepath.Join(tmp, filepath.FromSlash(w)),
					})
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		cleanup()
		return nil, func() {}, walkErr
	}
	return candidates, cleanup, nil
}

// loadPurchaseHints best-effort parses apple-import-reference.json (if present
// anywhere under treeRoot) and returns the lowercased basenames of the data
// files listed for any media-services bucket. These widen purchase-file
// detection beyond the built-in name patterns. Any error (missing file, bad
// JSON) yields an empty set; the import-reference is a hint, never a hard
// dependency.
func loadPurchaseHints(treeRoot string) map[string]bool {
	hints := map[string]bool{}

	var refPath string
	_ = filepath.WalkDir(treeRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Base(p), importReferenceName) {
			refPath = p
			return fs.SkipAll
		}
		return nil
	})
	if refPath == "" {
		return hints
	}

	data, err := os.ReadFile(refPath)
	if err != nil {
		return hints
	}
	ref, err := ParseImportReference(data)
	if err != nil {
		return hints
	}
	for mediaType, bucket := range ref {
		if !strings.Contains(strings.ToLower(mediaType), "media-services") {
			continue
		}
		for _, f := range bucket.DataFiles {
			hints[strings.ToLower(filepath.Base(filepath.ToSlash(f)))] = true
		}
	}
	return hints
}

// isPurchaseFile reports whether relName names a purchase-history CSV, by
// matching a known name pattern or appearing in the import-reference hints.
// Known non-purchase event logs (Purchase Flow/Server Events) are excluded.
// Hinted files still get header-gated downstream (a file yielding no titled
// rows is dropped), so a stray hint cannot manufacture a bogus purchase item.
func isPurchaseFile(relName string, hints map[string]bool) bool {
	base := strings.ToLower(filepath.Base(relName))
	if !strings.HasSuffix(base, ".csv") {
		return false
	}
	for _, ex := range purchaseExcludePatterns {
		if strings.Contains(base, ex) {
			return false
		}
	}
	if hints[base] {
		return true
	}
	for _, pat := range purchaseNamePatterns {
		if strings.Contains(base, pat) {
			return true
		}
	}
	return false
}

// readPurchases streams the CSV at absPath and returns one Purchase per data
// row that carries a title (rows without one are skipped as non-purchase
// noise).
func readPurchases(absPath string) ([]Purchase, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", absPath, err)
	}
	defer f.Close()

	var purchases []Purchase
	err = ReadCSV(f, func(row map[string]string) error {
		if p, ok := purchaseFromRow(row); ok {
			purchases = append(purchases, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return purchases, nil
}

// purchaseFromRow resolves a Purchase from one CSV row by header name (case-
// insensitive, with column fallbacks). It reports false when no title column is
// populated, signaling a non-purchase row to skip.
func purchaseFromRow(row map[string]string) (Purchase, bool) {
	lower := make(map[string]string, len(row))
	for k, v := range row {
		lower[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}

	p := Purchase{
		Title:    firstNonEmpty(lower, colTitle),
		Date:     firstNonEmpty(lower, colDate),
		Type:     firstNonEmpty(lower, colType),
		Cost:     firstNonEmpty(lower, colCost),
		Currency: firstNonEmpty(lower, colCurrency),
		Order:    firstNonEmpty(lower, colOrder),
		ItemRef:  firstNonEmpty(lower, colItemRef),
	}
	if p.Title == "" {
		return Purchase{}, false
	}
	return p, true
}

// firstNonEmpty returns the first non-empty value among the lowercased
// candidate column names, or "" if none are present.
func firstNonEmpty(lower map[string]string, candidates []string) string {
	for _, c := range candidates {
		if v, ok := lower[c]; ok && v != "" {
			return v
		}
	}
	return ""
}

// renderPurchasesMarkdown produces an LLM-readable markdown summary of one
// purchase file: a heading, the source filename, a stable total count, and a
// table of purchases. Cell values are sanitized so a pipe or newline in a title
// cannot break the table.
func renderPurchasesMarkdown(sourceFile string, purchases []Purchase) string {
	var b strings.Builder
	b.WriteString("# iTunes / App Store Purchases\n\n")
	fmt.Fprintf(&b, "Source file: %s\n\n", sourceFile)
	fmt.Fprintf(&b, "Total purchases: %d\n\n", len(purchases))
	b.WriteString("| Title | Date | Type | Cost | Currency | Order | Item Reference |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, p := range purchases {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
			mdCell(p.Title), mdCell(p.Date), mdCell(p.Type),
			mdCell(p.Cost), mdCell(p.Currency), mdCell(p.Order), mdCell(p.ItemRef))
	}
	return b.String()
}

// mdCell sanitizes a value for use inside a markdown table cell: pipes are
// escaped and newlines flattened. An empty value renders as "-".
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if s == "" {
		return "-"
	}
	return s
}
