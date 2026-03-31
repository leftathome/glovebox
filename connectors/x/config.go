package main

import "github.com/leftathome/glovebox/connector"

// Config is the full configuration for the X (Twitter) connector.
type Config struct {
	connector.BaseConfig
	UserID    string   `json:"user_id"`
	FeedTypes []string `json:"feed_types"` // e.g. ["mentions", "timeline"]
}
