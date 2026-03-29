package staging

import (
	"encoding/json"
	"io"
	"time"
)

// ItemMetadata represents the parsed metadata.json from a staging item.
type ItemMetadata struct {
	Source           string    `json:"source"`
	Sender           string    `json:"sender"`
	Subject          string    `json:"subject"`
	Timestamp        time.Time `json:"timestamp"`
	DestinationAgent string    `json:"destination_agent"`
	ContentType      string    `json:"content_type"`
	Ordered          bool      `json:"ordered"`
	AuthFailure      bool      `json:"auth_failure"`
}

// StagingItem represents a validated item read from the staging directory.
// Content is accessed via ContentPath (not loaded into memory) to support
// streaming scan with bounded memory.
type StagingItem struct {
	DirPath     string
	ContentPath string
	Metadata    ItemMetadata
}

// ParseMetadata reads and parses a metadata.json from the given reader.
func ParseMetadata(r io.Reader) (ItemMetadata, error) {
	var meta ItemMetadata
	if err := json.NewDecoder(r).Decode(&meta); err != nil {
		return ItemMetadata{}, err
	}
	return meta, nil
}
