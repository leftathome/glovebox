package main

import (
	"context"

	"github.com/leftathome/glovebox/connector"
)

// Config holds the Gmail connector configuration, embedding the framework's
// BaseConfig for route definitions.
type Config struct {
	connector.BaseConfig
	LabelIDs   []string `json:"label_ids"`   // default ["INBOX"]
	MaxResults int      `json:"max_results"` // default 25
}

// GmailMessage represents a single message returned by the Gmail API list+get flow.
type GmailMessage struct {
	ID           string
	Raw          []byte // decoded from base64url raw format
	Sender       string
	Subject      string
	InternalDate int64 // epoch milliseconds
}

// GmailClient abstracts Gmail API operations so the connector logic can be
// tested without real HTTP calls.
type GmailClient interface {
	// ListMessages returns message IDs matching the given label and query filter.
	ListMessages(ctx context.Context, labelID string, query string, maxResults int) ([]string, error)

	// GetMessage fetches a single message in raw format and returns it decoded.
	GetMessage(ctx context.Context, messageID string) (*GmailMessage, error)
}
