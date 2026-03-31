package main

import (
	"github.com/leftathome/glovebox/connector"
)

// Config is the full configuration for the Semantic Scholar connector.
type Config struct {
	connector.BaseConfig
	Queries []QueryConfig `json:"queries"`
}

// QueryConfig describes a single search query to poll.
type QueryConfig struct {
	Name  string `json:"name"`
	Query string `json:"query"`
}

// searchResponse represents the JSON response from the Semantic Scholar
// paper search API.
type searchResponse struct {
	Data []paper `json:"data"`
}

// paper represents a single paper returned by the API.
type paper struct {
	PaperID  string   `json:"paperId"`
	Title    string   `json:"title"`
	Abstract string   `json:"abstract"`
	TLDR     *tldr    `json:"tldr"`
	Authors  []author `json:"authors"`
	Year     int      `json:"year"`
}

// tldr represents the auto-generated summary of a paper.
type tldr struct {
	Text string `json:"text"`
}

// author represents a paper author.
type author struct {
	Name string `json:"name"`
}
