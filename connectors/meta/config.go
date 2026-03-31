package main

import "github.com/leftathome/glovebox/connector"

// Config is the full configuration for the Meta connector.
type Config struct {
	connector.BaseConfig
	PageID        string `json:"page_id"`
	FetchPosts    bool   `json:"fetch_posts"`
	FetchComments bool   `json:"fetch_comments"`
}
