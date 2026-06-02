package staging

import (
	"encoding/json"
	"io"
	"time"
)

// ItemIdentity represents the authenticated identity that produced an item.
type ItemIdentity struct {
	AccountID  string   `json:"account_id,omitempty"`
	Provider   string   `json:"provider"`
	AuthMethod string   `json:"auth_method"`
	Scopes     []string `json:"scopes,omitempty"`
	Tenant     string   `json:"tenant,omitempty"`
}

// ItemMetadata represents the parsed metadata.json from a staging item.
type ItemMetadata struct {
	Source           string            `json:"source"`
	Sender           string            `json:"sender"`
	Subject          string            `json:"subject"`
	Timestamp        time.Time         `json:"timestamp"`
	DestinationAgent string            `json:"destination_agent"`
	ContentType      string            `json:"content_type"`
	Ordered          bool              `json:"ordered"`
	AuthFailure      bool              `json:"auth_failure"`
	Identity         *ItemIdentity     `json:"identity,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
	DataSubject      string            `json:"data_subject,omitempty"`
	Audience         []string          `json:"audience,omitempty"`

	// Enrichments is the list of sidecar artifacts produced (or attempted
	// and failed) by the enrichment pipeline. Populated by
	// StagingItem.Commit(). Empty (nil) for items committed before this
	// field existed; this is an additive schema change. See
	// docs/specs/14-content-enrichment-design.md §4.6.
	Enrichments []EnrichmentRecord `json:"enrichments,omitempty"`
}

// EnrichmentRecord describes one sidecar artifact produced (or attempted
// and failed) by the enrichment pipeline, as recorded in metadata.json.
// See docs/specs/14-content-enrichment-design.md §4.6.
type EnrichmentRecord struct {
	// Producer is the Enricher.Name() that produced (or failed to produce)
	// this artifact (e.g. "html", "pdf", "ocr").
	Producer string `json:"producer"`
	// Kind is the artifact's semantic category (e.g. "extracted-text",
	// "image-caption", "ocr-text", "transcript"). Empty for failed
	// enrichments where the enricher never returned an artifact.
	Kind string `json:"kind"`
	// Source is the filename (relative to the item directory) the
	// enricher ran on, e.g. "content.raw" or "attachment-2-report.pdf".
	Source string `json:"source"`
	// Filename is the produced sidecar's filename (relative to the item
	// directory). Empty when Status="failed".
	Filename string `json:"filename,omitempty"`
	// Status is "ok" when the artifact was produced, "failed" when the
	// enricher returned a non-nil error.
	Status string `json:"status"`
	// Error is the enricher's error message; present iff Status="failed".
	Error string `json:"error,omitempty"`
}

// StagingItem represents a validated item read from the staging directory.
// Content is accessed via ContentPath (not loaded into memory) to support
// streaming scan with bounded memory.
type StagingItem struct {
	DirPath     string
	ContentPath string
	Metadata    ItemMetadata
}

func ParseMetadata(r io.Reader) (ItemMetadata, error) {
	var meta ItemMetadata
	if err := json.NewDecoder(r).Decode(&meta); err != nil {
		return ItemMetadata{}, err
	}
	return meta, nil
}
