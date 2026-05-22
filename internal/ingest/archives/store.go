// Package archives implements the tus.io resumable upload endpoint
// for archive-scale deliveries (multi-GB mbox / Takeout subtrees) per
// spec 13. This file owns the per-upload-id state store: upload-id
// generation, source-id binding (spec 13 §4.1), per-upload mutexes
// (spec 13 §4.4), and per-source + global concurrent-upload caps
// (spec 13 §5.4).
//
// The Store is in-memory; spec 13 §10 ("Out of Scope") explicitly
// rejects persisting upload state across process restarts in v1.
package archives

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"sync"
	"time"
)

// UploadState holds the per-upload-id state. The Mu protects the
// upload's mutable fields (Offset, Hasher, LastActivity) when a
// HEAD/PATCH/DELETE handler is operating on the upload. The Store's
// internal mutex (s.mu) is a separate lock that protects the maps;
// the Store never acquires UploadState.Mu itself -- that is the
// caller's responsibility (see spec 13 §4.4: the per-upload-id mutex
// is held by the PATCH/HEAD/DELETE handler, not by the store).
type UploadState struct {
	// Mu serializes concurrent access to this upload's mutable state.
	// PATCH/HEAD/DELETE handlers MUST hold Mu while touching Offset,
	// Hasher, or LastActivity. See spec 13 §4.4.
	Mu sync.Mutex

	// ID is the 128-bit hex upload-id (32 lowercase hex chars).
	ID string

	// SourceID is the authenticated source-id bound to this upload at
	// creation. Every subsequent HEAD/PATCH/DELETE MUST present the
	// same source-id; mismatches return ErrUploadNotFound (same error
	// as missing upload-id, no existence leak -- spec 13 §4.1).
	SourceID string

	// ArchiveID is the client-supplied stable identifier from the
	// Upload-Metadata header. Copied here for convenience; the
	// authoritative value lives in Meta.
	ArchiveID string

	// UploadLength is the declared total size in bytes (the
	// Upload-Length header value at POST time).
	UploadLength int64

	// Offset is the cumulative number of body bytes received so far.
	// Incremented by each successful PATCH. The upload transitions to
	// finalize when Offset == UploadLength.
	Offset int64

	// Hasher is the rolling sha256 hasher updated incrementally during
	// PATCH. At finalize, hex(Hasher.Sum(nil)) is compared against the
	// claimed sha256 from Upload-Metadata.
	Hasher hash.Hash

	// Meta is the validated Upload-Metadata. Defined by metadata.go
	// (Task B1).
	Meta *Metadata

	// CreatedAt is the wall-clock time of POST (UTC).
	CreatedAt time.Time

	// LastActivity is the wall-clock time of the most recent PATCH
	// (UTC). Used by the §5.5 cleanup goroutine to identify stale
	// uploads.
	LastActivity time.Time
}

// Store holds the live upload-id table and concurrent-upload counters.
// Methods are safe for concurrent use; the store-level mutex (s.mu)
// protects all map accesses.
type Store struct {
	mu      sync.Mutex
	uploads map[string]*UploadState // upload-id -> state
	perSrc  map[string]int          // source-id -> concurrent count
	cfg     StoreConfig
}

// StoreConfig configures the concurrent-upload caps. Both caps are
// hard caps: exceeding either returns an error from Create and the
// caller MUST surface a 429 with Retry-After (spec 13 §5.4).
type StoreConfig struct {
	// PerSourceMaxConcurrent caps the number of in-flight uploads from
	// any single source-id (default 4 per spec 13 §5.4).
	PerSourceMaxConcurrent int

	// GlobalMaxConcurrent caps the total number of in-flight uploads
	// across all source-ids (default 32 per spec 13 §5.4).
	GlobalMaxConcurrent int
}

// Sentinel errors returned by Store methods. Callers MUST inspect
// these with errors.Is to determine the HTTP response shape.
var (
	// ErrConcurrencyPerSource indicates the requesting source-id is
	// already at PerSourceMaxConcurrent in-flight uploads. The caller
	// surfaces a 429 + Retry-After 60 (spec 13 §5.4).
	ErrConcurrencyPerSource = errors.New("per-source concurrent upload cap exceeded")

	// ErrConcurrencyGlobal indicates the global in-flight cap is
	// reached. Same 429 + Retry-After semantics. Checked BEFORE the
	// per-source cap so a flood of distinct source-ids cannot bypass
	// the global cap by spreading themselves thin.
	ErrConcurrencyGlobal = errors.New("global concurrent upload cap exceeded")

	// ErrUploadNotFound indicates either (a) no upload exists with the
	// given upload-id, OR (b) the upload exists but is bound to a
	// different source-id. Returning the same error for both cases is
	// intentional: it prevents one source-id from probing the upload-id
	// space of another (spec 13 §4.1, the binding rule's no-leak
	// requirement).
	ErrUploadNotFound = errors.New("upload id not found or not owned by caller")

	// ErrUploadBusy indicates the per-upload-id mutex is currently
	// held by another PATCH/HEAD/DELETE handler. Surfaced by the
	// caller as 409 + {"error":"upload_busy"} per spec 13 §4.4.
	// The Store does not itself return this error from Get/Create/
	// Remove; it is defined here for the handler to use when its
	// trylock fails on UploadState.Mu.
	ErrUploadBusy = errors.New("upload busy")
)

// NewStore returns an empty Store configured with cfg.
func NewStore(cfg StoreConfig) *Store {
	return &Store{
		uploads: make(map[string]*UploadState),
		perSrc:  make(map[string]int),
		cfg:     cfg,
	}
}

// Create allocates a new upload-id bound to sourceID and registers an
// UploadState in the store. The new upload-id is 128-bit random hex
// (32 lowercase hex chars) generated from crypto/rand.
//
// Returns ErrConcurrencyGlobal if the global in-flight cap is reached
// (checked first, intentionally — see ErrConcurrencyGlobal docs);
// ErrConcurrencyPerSource if the per-source cap is reached; or a
// wrapped error from the underlying rand.Read call. A collision on
// the generated upload-id (vanishingly unlikely at 128 bits) returns
// a generic internal error which the C2a handler maps to 500.
func (s *Store) Create(sourceID string, m *Metadata, uploadLength int64) (*UploadState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Global cap is checked first so distinct source-ids cannot
	// collectively exceed the global cap by each staying under their
	// individual per-source caps.
	if len(s.uploads) >= s.cfg.GlobalMaxConcurrent {
		return nil, ErrConcurrencyGlobal
	}
	if s.perSrc[sourceID] >= s.cfg.PerSourceMaxConcurrent {
		return nil, ErrConcurrencyPerSource
	}

	id, err := newUploadID()
	if err != nil {
		return nil, err
	}
	if _, exists := s.uploads[id]; exists {
		// 128-bit random collision. v1 does not retry; surface as an
		// internal error and let the handler map to 500.
		return nil, fmt.Errorf("upload-id collision: %s", id)
	}

	var archiveID string
	if m != nil {
		archiveID = m.ArchiveID
	}

	now := time.Now().UTC()
	st := &UploadState{
		ID:           id,
		SourceID:     sourceID,
		ArchiveID:    archiveID,
		UploadLength: uploadLength,
		Meta:         m,
		Hasher:       sha256.New(),
		CreatedAt:    now,
		LastActivity: now,
	}
	s.uploads[id] = st
	s.perSrc[sourceID]++
	return st, nil
}

// Get returns the UploadState for id, but ONLY if it is bound to
// sourceID. Any other case (id missing, id bound to a different
// source-id) returns ErrUploadNotFound. Returning the same error
// for both cases prevents cross-source existence-leak fingerprinting
// (spec 13 §4.1 binding rule).
func (s *Store) Get(id, sourceID string) (*UploadState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.uploads[id]
	if !ok {
		return nil, ErrUploadNotFound
	}
	if st.SourceID != sourceID {
		return nil, ErrUploadNotFound
	}
	return st, nil
}

// Remove deletes the upload-id entry from the store and decrements
// the per-source concurrent counter. It is IDEMPOTENT: calling
// Remove on a non-existent upload-id is a no-op (no error). This
// simplifies cleanup-after-finalize paths where the caller may not
// know whether a prior cleanup has already run.
//
// Remove does NOT acquire UploadState.Mu; the caller MUST ensure no
// other handler is concurrently touching the state (typically Remove
// is invoked from the same goroutine that finished the final PATCH
// or processed a DELETE).
func (s *Store) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.uploads[id]
	if !ok {
		return
	}
	s.perSrc[st.SourceID]--
	if s.perSrc[st.SourceID] <= 0 {
		// Drop the map entry rather than leaving a zero counter so
		// the source-id namespace does not grow without bound.
		delete(s.perSrc, st.SourceID)
	}
	delete(s.uploads, id)
}

// newUploadID generates a 128-bit random upload-id and returns it
// hex-encoded (32 lowercase hex chars). Uses crypto/rand; never
// math/rand. A non-nil error from rand.Read is wrapped and returned
// (the handler maps this to 500).
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("upload-id rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
