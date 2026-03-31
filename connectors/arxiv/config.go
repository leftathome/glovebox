package main

import (
	"encoding/xml"

	"github.com/leftathome/glovebox/connector"
)

// Config is the full configuration for the Arxiv connector.
type Config struct {
	connector.BaseConfig
	Queries []QueryConfig `json:"queries"`
}

// QueryConfig describes a single Arxiv search query to poll.
type QueryConfig struct {
	Name       string `json:"name"`
	Query      string `json:"query"`       // e.g. "cat:cs.AI" or "all:transformer"
	MaxResults int    `json:"max_results"` // default 25
}

// arxivFeed represents the top-level Atom feed returned by the Arxiv API.
type arxivFeed struct {
	XMLName xml.Name     `xml:"feed"`
	Entries []arxivEntry `xml:"entry"`
}

// arxivEntry represents a single paper in the Arxiv Atom response.
type arxivEntry struct {
	ID         string          `xml:"id"`
	Title      string          `xml:"title"`
	Summary    string          `xml:"summary"`
	Published  string          `xml:"published"`
	Updated    string          `xml:"updated"`
	Authors    []arxivAuthor   `xml:"author"`
	Links      []arxivLink     `xml:"link"`
	Categories []arxivCategory `xml:"category"`
}

type arxivAuthor struct {
	Name string `xml:"name"`
}

type arxivLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type arxivCategory struct {
	Term string `xml:"term,attr"`
}
