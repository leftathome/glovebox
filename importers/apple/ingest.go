// Package main -- apple importer.
//
// This file builds the ingest envelope (connector.ItemOptions) for a single
// file entry from a finalized Apple-export staged archive tree. Apple
// deliveries are generic tarballs that carry no producer-asserted
// data_subject; the subject is supplied by config (data_subject_default) and
// threaded through the staging writer's merge chain. The rule matcher selects
// the destination agent and may additionally override data_subject / audience
// per rule (the glovebox-hyvp hardening pattern).
package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/ingest/archives"
)

// appleEntry represents a single file from the staged archive tree (under
// tree/). RelPath is the path relative to the tree/ root; ContentType is the
// MIME type classified by the caller (or via classifyContentType).
type appleEntry struct {
	RelPath     string
	ContentType string
}

// BuildItemOptions constructs the connector.ItemOptions for an Apple tree
// entry. The matcher determines DestinationAgent (failing if no rule matches,
// mirroring mbox/walhelm) and may carry per-rule data_subject / audience that
// the staging writer prefers over the config default. The apple_bucket tag
// records the producer-asserted matcher_id so downstream stages can route
// per Apple data category.
//
// fixedTags carries operator-configured tags (e.g. from --fixed-tags). Each
// entry is parsed as "key=value"; entries without "=" become a key with an
// empty value. Tag precedence: fixed tags first, then origin_archive and
// apple_bucket (which always win their keys).
//
// If no rule matches the entry's derived rule key and no wildcard fallback is
// configured, an error is returned describing the unset destination_agent
// condition (mirroring mbox/walhelm's early-fail pattern so the operator gets
// a clear diagnostic instead of a rejection downstream at the ingest handler).
func BuildItemOptions(
	entry appleEntry,
	receipt *archives.FinalizeReceipt,
	matcher *connector.RuleMatcher,
	sourceName string,
	fixedTags []string,
) (connector.ItemOptions, error) {
	if receipt == nil {
		return connector.ItemOptions{}, fmt.Errorf("build item options: nil receipt")
	}
	if matcher == nil {
		return connector.ItemOptions{}, fmt.Errorf("build item options: nil rule matcher")
	}

	ruleKey := ruleKeyForEntry(entry)
	result, matched := matcher.Match(ruleKey)
	if !matched {
		return connector.ItemOptions{}, fmt.Errorf(
			"no destination-agent rule matched key %q and no wildcard fallback is configured; "+
				"destination_agent would be unset (ingest requires it)",
			ruleKey,
		)
	}
	if result.Destination == "" {
		return connector.ItemOptions{}, fmt.Errorf(
			"rule matched for key %q but destination is empty; "+
				"destination_agent would be unset (ingest requires it)",
			ruleKey,
		)
	}

	tags := make(map[string]string)
	for _, raw := range fixedTags {
		k, v := parseFixedTag(raw)
		if k == "" {
			continue
		}
		tags[k] = v
	}
	// origin_archive and apple_bucket always win their keys regardless of
	// fixedTags.
	tags["origin_archive"] = receipt.ArchiveID + ":" + entry.RelPath
	tags["apple_bucket"] = receipt.MatcherID

	// Sender / Timestamp are required by the staging metadata validator
	// (internal/staging.Validate). Apple deliveries carry no per-message
	// sender, so the uploading principal (DeliveredBy, falling back to the
	// source name) stands in; Timestamp is when ingest received the archive
	// (ReceivedAt, falling back to now so an unset receipt clock does not
	// block staging).
	sender := receipt.DeliveredBy
	if sender == "" {
		sender = sourceName
	}
	timestamp := receipt.ReceivedAt
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}

	opts := connector.ItemOptions{
		Source:           sourceName,
		Sender:           sender,
		Timestamp:        timestamp,
		DestinationAgent: result.Destination,
		ContentType:      entry.ContentType,
		Tags:             tags,
		RuleTags:         result.Tags,
		// Carry the matched rule's data_subject / audience so an operator can
		// route a specific Apple bucket to a particular person's agent. Empty
		// values fall through to the operator-configured defaults
		// (data_subject_default / audience_default).
		RuleDataSubject: result.DataSubject,
		RuleAudience:    result.Audience,
	}

	return opts, nil
}

// ruleKeyForEntry derives a rule-matcher key from an appleEntry so operators
// can route per data-category. The key has the form "apple:<type>" where
// <type> is:
//
//   - The top-level directory component of RelPath if the path has at least
//     one directory separator (e.g., "media-services/plays.csv" ->
//     "apple:media-services").
//   - The filename stem (without extension) of a file directly in tree/ root
//     if the path has no separator (e.g., "export.json" -> "apple:export").
//
// This keeps routing config simple: one rule per data-category folder.
func ruleKeyForEntry(entry appleEntry) string {
	rel := filepath.ToSlash(entry.RelPath)
	if i := strings.IndexByte(rel, '/'); i > 0 {
		return "apple:" + rel[:i]
	}
	base := filepath.Base(rel)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return "apple:" + stem
}

// parseFixedTag splits "key=value" into (key, value). An entry without "="
// returns (trimmed, "") so callers can express bare keys (flag-only tags). An
// empty or blank raw string returns ("", "").
func parseFixedTag(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	if i := strings.IndexByte(raw, '='); i >= 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+1:])
	}
	return raw, ""
}

// classifyContentType returns the MIME type for a file based solely on its
// extension. Callers use this when populating appleEntry.ContentType before
// calling BuildItemOptions.
//
// Mapping:
//
//	.csv  -> text/csv
//	.json -> application/json
//	.pdf  -> application/pdf
//	else  -> application/octet-stream
func classifyContentType(relPath string) string {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	case ".pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}
