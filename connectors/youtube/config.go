package main

import "github.com/leftathome/glovebox/connector"

// Config is the full configuration for the YouTube connector.
type Config struct {
	connector.BaseConfig
	ChannelIDs    []string `json:"channel_ids"`
	FetchComments *bool    `json:"fetch_comments,omitempty"` // default true
	MaxComments   int      `json:"max_comments,omitempty"`   // default 25
	FetchCaptions *bool    `json:"fetch_captions,omitempty"` // default true
}

// ShouldFetchComments returns whether comments should be fetched (default true).
func (cfg Config) ShouldFetchComments() bool {
	if cfg.FetchComments == nil {
		return true
	}
	return *cfg.FetchComments
}

// ShouldFetchCaptions returns whether caption metadata should be fetched (default true).
func (cfg Config) ShouldFetchCaptions() bool {
	if cfg.FetchCaptions == nil {
		return true
	}
	return *cfg.FetchCaptions
}

// EffectiveMaxComments returns the configured max comments or 25 as default.
func (cfg Config) EffectiveMaxComments() int {
	if cfg.MaxComments > 0 {
		return cfg.MaxComments
	}
	return 25
}
