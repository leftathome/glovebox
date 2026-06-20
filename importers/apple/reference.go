// reference.go -- parser for the apple/import-reference deliverable
// (glovebox-yea7). Every Apple ingest delivers an apple/import-reference bucket
// containing apple-import-reference.json: a JSON object keyed by recognizer
// media_type, each value describing one delivered bucket (the files it holds,
// the Apple guide documents that explain its columns, and import caveats). See
// docs/handoffs/recognizer-apple-export-delivery.md SS5.
//
// This is a pure parser. It is intentionally NOT wired into importer.go; the
// consumer (glovebox-ot7v) parses apple-import-reference.json first to drive
// ingestion of the other buckets.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// BucketRef describes one delivered Apple bucket: the data files it holds, the
// Apple field-glossary guide documents relevant to it, and free-form import
// caveats. It is the value type of the apple-import-reference.json map.
type BucketRef struct {
	DataFiles []string `json:"data_files"`
	GuideRefs []string `json:"guide_refs"`
	Notes     string   `json:"notes"`
}

// ImportReference is the parsed apple-import-reference.json: a map keyed by
// recognizer media_type (e.g. "archive/apple/media-services") to its BucketRef.
type ImportReference map[string]BucketRef

// ErrEmptyImportReference is returned by ParseImportReference when given no
// (or whitespace-only) input.
var ErrEmptyImportReference = errors.New("apple: empty import-reference input")

// ParseImportReference unmarshals an apple-import-reference.json body into an
// ImportReference. It returns ErrEmptyImportReference on empty or
// whitespace-only input and a descriptive error on malformed JSON or a body
// that is not a JSON object.
func ParseImportReference(data []byte) (ImportReference, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, ErrEmptyImportReference
	}

	var ref ImportReference
	if err := json.Unmarshal(data, &ref); err != nil {
		return nil, fmt.Errorf("apple: parse import-reference: %w", err)
	}

	return ref, nil
}

// Bucket looks up the BucketRef for a recognizer media_type, reporting whether
// it was present.
func (m ImportReference) Bucket(mediaType string) (BucketRef, bool) {
	b, ok := m[mediaType]
	return b, ok
}
