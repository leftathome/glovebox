package main

import (
	"context"

	"github.com/leftathome/glovebox/connector"
)

// Config holds the Outlook connector configuration, embedding the framework's
// BaseConfig for route definitions.
type Config struct {
	connector.BaseConfig
	FolderIDs  []string `json:"folder_ids"`  // default ["inbox"]
	MaxResults int      `json:"max_results"` // default 25
}

// OutlookMessage represents a single message returned by the Microsoft Graph
// mail API.
type OutlookMessage struct {
	ID               string
	Subject          string
	From             string
	ReceivedDateTime string // ISO 8601 format
	BodyContent      string // plain text body (HTML already converted)
	BodyContentType  string // "text" or "html" from the API
}

// OutlookClient abstracts Microsoft Graph mail API operations so the connector
// logic can be tested without real HTTP calls.
type OutlookClient interface {
	// ListMessages returns messages from the given folder that are newer
	// than the checkpoint timestamp. Results are ordered by receivedDateTime.
	ListMessages(ctx context.Context, folderID string, checkpoint string, maxResults int) ([]OutlookMessage, error)
}
