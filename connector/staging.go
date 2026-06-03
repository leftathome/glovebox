package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/leftathome/glovebox/connector/enrich"
	"github.com/leftathome/glovebox/internal/staging"
)

type ItemOptions struct {
	Source           string
	Sender           string
	Subject          string
	Timestamp        time.Time
	DestinationAgent string
	ContentType      string
	Ordered          bool
	AuthFailure      bool
	Identity         *Identity
	Tags             map[string]string
	RuleTags         map[string]string
	DataSubject      string
	Audience         []string
	RuleDataSubject  string
	RuleAudience     []string
}

type StagingWriter struct {
	stagingDir               string
	connectorName            string
	tmpDir                   string
	configIdentity           *ConfigIdentity
	configDataSubjectDefault string
	configAudienceDefault    []string
	// enrichRegistry is the enrichment registry consulted by
	// StagingItem.Commit(). Nil means "use enrich.Default" (the
	// process-global registry); tests inject a fresh registry to avoid
	// leaking state through enrich.Default. See
	// docs/specs/14-content-enrichment-design.md §4.3.
	enrichRegistry *enrich.Registry
}

func NewStagingWriter(stagingDir string, connectorName string) (*StagingWriter, error) {
	tmpDir := filepath.Join(stagingDir, ".tmp", connectorName)
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return nil, fmt.Errorf("create staging tmp dir: %w", err)
	}
	return &StagingWriter{
		stagingDir:    stagingDir,
		connectorName: connectorName,
		tmpDir:        tmpDir,
	}, nil
}

// SetConfigIdentity sets the config-level identity used as the base for
// identity merging at Commit() time.
func (w *StagingWriter) SetConfigIdentity(ci *ConfigIdentity) {
	w.configIdentity = ci
}

// SetConfigDataSubject sets the config-level data_subject default used as
// the final fallback in the merge chain (spec 11 §5).
func (w *StagingWriter) SetConfigDataSubject(s string) {
	w.configDataSubjectDefault = s
}

// SetConfigAudience sets the config-level audience default used as the
// final fallback in the merge chain (spec 11 §5). The slice is copied to
// prevent aliasing into the caller's storage.
func (w *StagingWriter) SetConfigAudience(a []string) {
	if len(a) == 0 {
		w.configAudienceDefault = nil
		return
	}
	w.configAudienceDefault = append([]string(nil), a...)
}

// SetEnrichRegistry overrides the enrichment registry used by items
// produced by this writer. When unset (or set to nil), Commit() uses
// the process-global enrich.Default registry. Tests inject a fresh
// registry here to avoid leaking state through enrich.Default.
func (w *StagingWriter) SetEnrichRegistry(r *enrich.Registry) {
	w.enrichRegistry = r
}

type StagingItem struct {
	dir                      string
	stagingDir               string
	opts                     ItemOptions
	configIdentity           *ConfigIdentity
	configDataSubjectDefault string
	configAudienceDefault    []string
	enrichRegistry           *enrich.Registry
	commitFunc               func() error
}

func (w *StagingWriter) NewItem(opts ItemOptions) (*StagingItem, error) {
	name := fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102-150405"), uuid.New().String()[:8])
	dir := filepath.Join(w.tmpDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create item dir: %w", err)
	}
	return &StagingItem{
		dir:                      dir,
		stagingDir:               w.stagingDir,
		opts:                     opts,
		configIdentity:           w.configIdentity,
		configDataSubjectDefault: w.configDataSubjectDefault,
		configAudienceDefault:    w.configAudienceDefault,
		enrichRegistry:           w.enrichRegistry,
	}, nil
}

// WriteContent writes (or appends) content to content.raw.
// For streaming, use ContentWriter() instead.
func (si *StagingItem) WriteContent(data []byte) error {
	w, err := si.contentFile()
	if err != nil {
		return err
	}
	defer w.Close()
	_, err = w.Write(data)
	return err
}

func (si *StagingItem) ContentWriter() (io.WriteCloser, error) {
	return si.contentFile()
}

func (si *StagingItem) contentFile() (*os.File, error) {
	path := filepath.Join(si.dir, "content.raw")
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}

// buildMetadata constructs and validates ItemMetadata from the staging item's
// options, merging tags and identity. Used by both filesystem and HTTP backends.
func (si *StagingItem) buildMetadata() (staging.ItemMetadata, error) {
	meta := staging.ItemMetadata{
		Source:           si.opts.Source,
		Sender:           si.opts.Sender,
		Subject:          staging.StripSubjectControlChars(si.opts.Subject),
		Timestamp:        si.opts.Timestamp,
		DestinationAgent: si.opts.DestinationAgent,
		ContentType:      si.opts.ContentType,
		Ordered:          si.opts.Ordered,
		AuthFailure:      si.opts.AuthFailure,
	}

	mergedTags := mergeTags(si.opts.RuleTags, si.opts.Tags)
	if len(mergedTags) > 0 {
		meta.Tags = mergedTags
	}

	mergedIdentity := MergeIdentity(si.configIdentity, si.opts.Identity)
	if mergedIdentity != nil {
		meta.Identity = &staging.ItemIdentity{
			AccountID:  mergedIdentity.AccountID,
			Provider:   mergedIdentity.Provider,
			AuthMethod: mergedIdentity.AuthMethod,
			Scopes:     mergedIdentity.Scopes,
			Tenant:     mergedIdentity.Tenant,
		}
	}

	meta.DataSubject = mergeDataSubject(si.configDataSubjectDefault, si.opts.RuleDataSubject, si.opts.DataSubject)
	meta.Audience = mergeAudience(si.configAudienceDefault, si.opts.RuleAudience, si.opts.Audience)

	if meta.DestinationAgent == "" {
		return staging.ItemMetadata{}, fmt.Errorf("metadata validation: destination_agent is required")
	}
	allowlist := []string{meta.DestinationAgent}
	if errs := staging.Validate(meta, allowlist); len(errs) > 0 {
		return staging.ItemMetadata{}, fmt.Errorf("metadata validation: %v", errs)
	}

	return meta, nil
}

// Commit finalizes the staging item.
//
// Backend asymmetry (KNOWN GAP, spec 14 follow-on): when the connector
// is configured with HTTPStagingBackend (which sets si.commitFunc), the
// commit path POSTs the raw content to the ingest server and the local
// enrichment pipeline is SKIPPED. Server-side enrichment (if any) runs
// on the ingest server. Downstream consumers that read metadata.json
// CANNOT distinguish "this came via HTTP, enrichment ran server-side"
// from "this is a legacy item, enrichment didn't run." Threading the
// enrichment pipeline through the HTTP path (or relying on server-side
// enrichment with parity) is tracked separately.
func (si *StagingItem) Commit() error {
	if si.commitFunc != nil {
		return si.commitFunc()
	}

	meta, err := si.buildMetadata()
	if err != nil {
		os.RemoveAll(si.dir)
		return err
	}

	// ---- enrichment pipeline (spec 14 §4.3) ----
	// Per-enricher errors do NOT abort the commit (§4.4): they are
	// captured into meta.Enrichments[] with Status="failed" and a
	// content.<enricher>.error.md marker file is written per failure.
	si.runEnrichmentPipeline(&meta)
	// ---- end pipeline ----

	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(si.dir, "metadata.json"), data, 0644); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}

	destDir := filepath.Join(si.stagingDir, filepath.Base(si.dir))
	if err := os.Rename(si.dir, destDir); err != nil {
		return fmt.Errorf("atomic rename: %w", err)
	}

	return nil
}

// runEnrichmentPipeline runs the active enrichment registry against the
// primary content file (content.raw) and every attachment-* sibling,
// records the resulting artifacts and errors into meta.Enrichments, and
// writes per-failure marker files. Failures here NEVER abort the
// commit; an enrichment marker file is the worst case.
func (si *StagingItem) runEnrichmentPipeline(meta *staging.ItemMetadata) {
	registry := si.enrichRegistry
	if registry == nil {
		registry = enrich.Default
	}

	// StagingItem does not currently carry a context.Context — Commit
	// is not context-aware in the existing API. For v1 we pass
	// context.Background(); a follow-on bead can thread a real context
	// through if/when connectors gain a per-item deadline contract.
	ctx := context.Background()

	contentPath := filepath.Join(si.dir, "content.raw")
	var records []enrich.EnrichmentRecord

	// Resolve the attachment set BEFORE primary enrichment so that any
	// primary-enricher sidecars whose names happen to match `attachment-*`
	// (e.g., a hypothetical enricher that writes `attachment-summary.md`)
	// cannot accidentally be picked up as attachments themselves. Sort
	// for deterministic order; filepath.Glob's documented order is
	// lexicographical but the explicit sort keeps the contract obvious.
	attachments, _ := filepath.Glob(filepath.Join(si.dir, "attachment-*"))
	sort.Strings(attachments)

	// Primary source. Spec §4.3 example calls ApplyAll unconditionally;
	// we guard with os.Stat so connectors that produce attachment-only
	// items (no content.raw) don't pass a non-existent path through to
	// enrichers.
	var primaryErrs []enrich.EnricherError
	if _, statErr := os.Stat(contentPath); statErr == nil {
		primaryArts, errs := registry.ApplyAll(ctx, contentPath, *meta, si.dir)
		records = enrich.AppendRecords(records, "content.raw", primaryArts, errs)
		primaryErrs = errs
	}

	// Attachments (set was captured above pre-primary-enrichment).
	var attachmentErrs []enrich.EnricherError
	for _, ap := range attachments {
		arts, errs := registry.ApplyAll(ctx, ap, *meta, si.dir)
		records = enrich.AppendRecords(records, filepath.Base(ap), arts, errs)
		attachmentErrs = append(attachmentErrs, errs...)
	}

	if len(records) > 0 {
		meta.Enrichments = records
	}

	if err := enrich.WriteErrorMarkers(si.dir, primaryErrs, attachmentErrs); err != nil {
		// Non-fatal: log and proceed. The records in meta.Enrichments
		// already carry the failure detail, so this is purely about
		// the marker sidecar files.
		log.Printf("staging: failed to write enrichment error markers in %s: %v", si.dir, err)
	}
}

// mergeTags merges rule-level tags with per-item tags.
// Per-item tags win on key conflict. Returns nil if both are empty.
func mergeTags(ruleTags, itemTags map[string]string) map[string]string {
	if len(ruleTags) == 0 && len(itemTags) == 0 {
		return nil
	}
	merged := make(map[string]string, len(ruleTags)+len(itemTags))
	for k, v := range ruleTags {
		merged[k] = v
	}
	for k, v := range itemTags {
		merged[k] = v
	}
	return merged
}

// mergeDataSubject applies per-item > rule > config default > empty precedence
// per spec 11 §5.
func mergeDataSubject(configDefault, rule, item string) string {
	if item != "" {
		return item
	}
	if rule != "" {
		return rule
	}
	return configDefault
}

// mergeAudience applies per-item > rule > config default > nil precedence
// per spec 11 §5. Returns a fresh slice to prevent aliasing.
func mergeAudience(configDefault, rule, item []string) []string {
	switch {
	case len(item) > 0:
		return append([]string(nil), item...)
	case len(rule) > 0:
		return append([]string(nil), rule...)
	case len(configDefault) > 0:
		return append([]string(nil), configDefault...)
	}
	return nil
}

func (w *StagingWriter) CleanOrphans() {
	entries, err := os.ReadDir(w.tmpDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			os.RemoveAll(filepath.Join(w.tmpDir, e.Name()))
		}
	}
}
