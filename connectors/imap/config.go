package main

import (
	"context"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// Config holds the IMAP connector configuration, embedding the framework's
// BaseConfig for route definitions.
type Config struct {
	connector.BaseConfig
	Folders []FolderConfig `json:"folders"`
}

// FolderConfig identifies a single IMAP folder to poll.
type FolderConfig struct {
	Name string `json:"name"`
}

// IMAPClient abstracts IMAP operations so the connector logic can be tested
// without a real IMAP server.
type IMAPClient interface {
	Connect(ctx context.Context) error
	SelectFolder(ctx context.Context, folder string) error
	SearchSinceUID(ctx context.Context, uid uint32) ([]uint32, error)
	FetchMessage(ctx context.Context, uid uint32) (raw []byte, sender string, subject string, date time.Time, err error)
	Idle(ctx context.Context) error
	Close() error
}
