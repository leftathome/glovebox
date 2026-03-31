package main

import "github.com/leftathome/glovebox/connector"

// Config is the full configuration for the GitHub connector.
type Config struct {
	connector.BaseConfig
	Repos         []RepoConfig `json:"repos"`
	WebhookSecret string       `json:"webhook_secret_env"` // env var name holding the secret
}

// RepoConfig describes a single GitHub repository to poll.
type RepoConfig struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
}
