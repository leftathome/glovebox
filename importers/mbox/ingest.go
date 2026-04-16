// Package main -- mbox importer.
//
// This file builds the ingest envelope (connector.ItemOptions) for a parsed
// mbox *Message. It applies the destination-agent rule system described in
// docs/specs/09-mbox-importer-design.md §3.5 and §3.10.
//
// The bead (glovebox-afb) owns only envelope construction; the actual multipart
// POST to /v1/ingest is performed by the framework's Backend (HTTPStagingBackend
// or StagingWriter) and is wired up by the worker-pool bead.
package main

import (
	"fmt"
	"strings"

	"github.com/leftathome/glovebox/connector"
)

// BuildItemOptions constructs the connector.ItemOptions that the framework's
// Backend needs to stage or POST an mbox message.
//
// The rule-match input string is chosen, in priority order, from the parsed
// message headers:
//
//  1. If the message has one or more X-Gmail-Labels, the first label is used
//     as "label:<Label>" (matching the gmail connector's convention; see
//     connectors/gmail/connector.go).
//  2. Otherwise, if List-Id (or List-Post) is present, "list_id:<ListID>" is
//     used.
//  3. Otherwise, if From is present, "sender:<From>" is used.
//  4. Otherwise, the empty string is used -- which will only match a wildcard
//     ("*") rule in the matcher.
//
// Operators configure rules in the importer's main config identical in shape
// to connectors/imap/config.json (see §3.5).
//
// If no rule matches and the matcher has no wildcard fallback, the returned
// error describes that destination_agent would be unset -- which the ingest
// handler would reject anyway; the importer fails early here for a clearer
// diagnostic.
//
// fixedTags carries operator-configured tags supplied by the importer's main
// config. Each entry is parsed as "key=value"; entries without "=" become a
// key with an empty value. Duplicate keys in the slice: last wins (matching
// map-merge semantics).
//
// Tag precedence (last wins, matching connector.mergeTags behavior):
//
//	ruleTags (from RuleMatcher) -> fixedTags (operator config) -> origin_archive
//
// The origin_archive tag always wins for its key; it has the form
// {"origin_archive": "<source-name>:<byte-offset>"} and provides traceability
// from any ingested item back to its mbox position (§3.10).
func BuildItemOptions(
	m *Message,
	ruleMatcher *connector.RuleMatcher,
	sourceName string,
	fixedTags []string,
) (connector.ItemOptions, error) {
	if m == nil {
		return connector.ItemOptions{}, fmt.Errorf("build item options: nil message")
	}
	if ruleMatcher == nil {
		return connector.ItemOptions{}, fmt.Errorf("build item options: nil rule matcher")
	}

	ruleKey := mailRuleKey(m)
	result, matched := ruleMatcher.Match(ruleKey)
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

	// Build the merged tag map. Precedence (last wins): rule tags, then
	// fixed tags, then origin_archive. RuleTags is kept separate on the
	// ItemOptions so downstream metadata construction records the
	// rule-provided provenance; Tags carries the per-item additions
	// (fixed tags + origin_archive).
	itemTags := make(map[string]string)
	for _, raw := range fixedTags {
		k, v := parseFixedTag(raw)
		if k == "" {
			// Skip malformed / empty-key entries rather than letting
			// them fail validation downstream.
			continue
		}
		itemTags[k] = v
	}
	itemTags["origin_archive"] = fmt.Sprintf("%s:%d", sourceName, m.ByteOffset)

	opts := connector.ItemOptions{
		Source:           sourceName,
		Sender:           m.From,
		Subject:          m.Subject,
		Timestamp:        m.Date,
		DestinationAgent: result.Destination,
		ContentType:      "message/rfc822",
		Tags:             itemTags,
		RuleTags:         result.Tags,
	}

	return opts, nil
}

// mailRuleKey returns the rule-matcher input string for an mbox *Message,
// in priority order: Gmail label -> List-Id -> sender address -> empty.
func mailRuleKey(m *Message) string {
	if len(m.GmailLabels) > 0 {
		return "label:" + m.GmailLabels[0]
	}
	if m.ListID != "" {
		return "list_id:" + m.ListID
	}
	if m.From != "" {
		return "sender:" + m.From
	}
	return ""
}

// parseFixedTag splits "key=value" into (key, value). An entry without "="
// returns (trimmed, "") so callers can express bare keys.
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
