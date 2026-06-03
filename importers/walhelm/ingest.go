// Package main -- walhelm importer.
//
// This file builds the ingest envelope (connector.ItemOptions) for a single
// file entry from a walhelm staged archive tree. Subject, audience, and
// identity come exclusively from the producer-asserted FinalizeReceipt; the
// rule matcher is consulted ONLY to determine the destination agent.
//
// This is the defining architectural difference from the mbox importer, which
// derives subject/audience from the message headers and rule config. For
// walhelm, the ingest pipeline stamped provenance at upload time (spec 15
// sec 6); re-deriving it from message content would be wrong.
package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/ingest/archives"
)

// walhelmEntry represents a single file from the staged archive tree (under
// tree/). RelPath is the path relative to the tree/ root; ContentType is the
// MIME type classified by the caller (or via classifyContentType).
type walhelmEntry struct {
	RelPath     string
	ContentType string
}

// BuildItemOptions constructs the connector.ItemOptions for a walhelm tree
// entry. Provenance (DataSubject, Audience, Identity) is stamped from receipt,
// not from the matcher. The matcher determines DestinationAgent only.
//
// If no rule matches the entry's derived rule key and no wildcard fallback is
// configured, an error is returned describing the unset destination_agent
// condition (mirroring mbox's early-fail pattern so the operator gets a clear
// diagnostic instead of a rejection downstream at the ingest handler).
func BuildItemOptions(
	entry walhelmEntry,
	receipt *archives.FinalizeReceipt,
	matcher *connector.RuleMatcher,
	sourceName string,
) (connector.ItemOptions, error) {
	if receipt == nil {
		return connector.ItemOptions{}, fmt.Errorf("build item options: nil receipt")
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

	// Identity comes from receipt.Acquisition, NOT from the rule result.
	// Guard nil Acquisition: leave Identity nil rather than panicking.
	var identity *connector.Identity
	if receipt.Acquisition != nil {
		identity = &connector.Identity{
			Provider:   receipt.Acquisition.Provider,
			AuthMethod: receipt.Acquisition.AuthMethod,
			AccountID:  receipt.Acquisition.AccountID,
		}
	}

	// Audience is copied from receipt to prevent aliasing.
	audience := append([]string(nil), receipt.Audience...)

	tags := map[string]string{
		"origin_archive": receipt.ArchiveID + ":" + entry.RelPath,
	}

	// Sender and Timestamp are required by the staging metadata validator
	// (internal/staging.Validate). For walhelm the producer-asserted
	// provenance is the authority: Sender is the uploading principal
	// (DeliveredBy, falling back to the acquisition account), and
	// Timestamp is when ingest received the archive (ReceivedAt, falling
	// back to now so an unset receipt clock does not block staging).
	sender := receipt.DeliveredBy
	if sender == "" && receipt.Acquisition != nil {
		sender = receipt.Acquisition.AccountID
	}
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
		Identity:         identity,
		DataSubject:      receipt.DataSubject,
		Audience:         audience,
		Tags:             tags,
	}

	return opts, nil
}

// ruleKeyForEntry derives a rule-matcher key from a walhelmEntry so operators
// can route per data-type. The key has the form "walhelm:<type>" where <type>
// is:
//
//   - The top-level directory component of RelPath if the path has at least
//     one directory separator (e.g., "message/inbox/mail.eml" -> "walhelm:message").
//   - The filename stem (without extension) of a file directly in tree/ root
//     if the path has no separator (e.g., "record.json" -> "walhelm:record").
//
// This keeps routing config simple: one rule per data-category folder.
func ruleKeyForEntry(entry walhelmEntry) string {
	rel := filepath.ToSlash(entry.RelPath)
	if i := strings.IndexByte(rel, '/'); i > 0 {
		return "walhelm:" + rel[:i]
	}
	// File at tree/ root: use stem.
	base := filepath.Base(rel)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return "walhelm:" + stem
}

// classifyContentType returns the MIME type for a file based solely on its
// extension. Callers use this when populating walhelmEntry.ContentType before
// calling BuildItemOptions.
//
// Mapping:
//
//	.eml  -> message/rfc822
//	.json -> application/json
//	.zip  -> application/zip
//	else  -> application/octet-stream
func classifyContentType(relPath string) string {
	switch strings.ToLower(filepath.Ext(relPath)) {
	case ".eml":
		return "message/rfc822"
	case ".json":
		return "application/json"
	case ".zip":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

