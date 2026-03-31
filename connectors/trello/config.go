package main

import (
	"github.com/leftathome/glovebox/connector"
)

// Config is the full configuration for the Trello connector.
type Config struct {
	connector.BaseConfig
	Boards []BoardConfig `json:"boards"`
}

// BoardConfig describes a single Trello board to poll.
type BoardConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
