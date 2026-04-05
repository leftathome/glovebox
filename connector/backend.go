package connector

// StagingBackend delivers completed staging items to the glovebox scanner.
// The existing StagingWriter implements this interface using the filesystem.
// Future backends (e.g. HTTP ingest) will provide alternative implementations.
type StagingBackend interface {
	// NewItem creates a new staging item for content accumulation.
	// The caller writes content via StagingItem.WriteContent (or ContentWriter)
	// and finalizes with StagingItem.Commit().
	NewItem(opts ItemOptions) (*StagingItem, error)

	// SetConfigIdentity sets default identity fields for all items.
	// Per-item Identity fields override these defaults at Commit time.
	SetConfigIdentity(ci *ConfigIdentity)
}

// Compile-time check: *StagingWriter satisfies StagingBackend.
var _ StagingBackend = (*StagingWriter)(nil)
