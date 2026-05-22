// Package archives test helpers.
//
// Ownership: this file is owned by Task B2 (the store implementation)
// per the spec-13 plan §Test helpers. Subsequent tasks within the
// archives package (B3 untar, B4 telemetry, C2a/b handlers) add their
// own helpers here rather than redefining. Keep the helpers minimal
// and dependency-free; this file MUST NOT pull in package-external
// fixtures.

package archives

import (
	"testing"
)

// mustNewUploadState returns an *UploadState suitable for tests that
// need a populated state without exercising the full Store.Create
// path. It constructs a minimal *Metadata struct-literal — B1
// (metadata.go) defines the Metadata type and may evolve it; this
// fixture relies only on the ArchiveID field, which the spec requires
// to exist.
//
// The returned state has its Hasher and timestamps unset; callers
// that need them should set them explicitly or use Store.Create.
func mustNewUploadState(t *testing.T, sourceID, archiveID string, uploadLength int64) *UploadState {
	t.Helper()
	return &UploadState{
		ID:           "test-upload-id",
		SourceID:     sourceID,
		ArchiveID:    archiveID,
		UploadLength: uploadLength,
		Meta:         &Metadata{ArchiveID: archiveID},
	}
}
