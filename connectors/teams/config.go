package main

import "github.com/leftathome/glovebox/connector"

// Config is the full configuration for the Teams connector.
type Config struct {
	connector.BaseConfig
	Channels []ChannelConfig `json:"channels"`
}

// ChannelConfig describes a single Teams channel to poll for messages.
type ChannelConfig struct {
	TeamID    string `json:"team_id"`
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"` // display name for routing
}
