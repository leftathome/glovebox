package main

import "github.com/leftathome/glovebox/connector"

// Config is the full configuration for the YouTube connector.
type Config struct {
	connector.BaseConfig
	ChannelIDs []string `json:"channel_ids"`
}
