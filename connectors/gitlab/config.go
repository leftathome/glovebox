package main

import (
	"github.com/leftathome/glovebox/connector"
)

// Config is the full configuration for the GitLab connector.
type Config struct {
	connector.BaseConfig
	Projects []ProjectConfig `json:"projects"`
	BaseURL  string          `json:"base_url"` // default: https://gitlab.com
}

// ProjectConfig describes a single GitLab project to poll for events.
type ProjectConfig struct {
	Path string `json:"path"` // e.g. "mygroup/myproject"
}
