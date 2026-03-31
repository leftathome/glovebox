package main

import "github.com/leftathome/glovebox/connector"

// Config is the full configuration for the Notion connector.
type Config struct {
	connector.BaseConfig
	DatabaseIDs []string `json:"database_ids"`
	PageIDs     []string `json:"page_ids"`
}
