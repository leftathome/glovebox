package main

import (
	"github.com/leftathome/glovebox/connector"
)

// Config is the full configuration for the Bluesky connector.
type Config struct {
	connector.BaseConfig
	Service  string   `json:"service"`    // default "https://bsky.social"
	FeedURIs []string `json:"feed_uris"`  // e.g. ["at://did:plc:.../app.bsky.feed.getAuthorFeed"]
}
