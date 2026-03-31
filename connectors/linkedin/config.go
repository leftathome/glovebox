package main

import "github.com/leftathome/glovebox/connector"

// Config is the full configuration for the LinkedIn connector.
type Config struct {
	connector.BaseConfig
	FeedTypes []string `json:"feed_types"` // e.g. ["posts", "shares"]
}
