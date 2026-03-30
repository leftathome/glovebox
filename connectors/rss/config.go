package main

import (
	"encoding/xml"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/content"
)

// Config is the full configuration for the RSS connector.
type Config struct {
	connector.BaseConfig
	Feeds      []FeedConfig             `json:"feeds"`
	FetchLinks bool                     `json:"fetch_links"`
	LinkPolicy content.LinkPolicyConfig `json:"link_policy"`
}

// FeedConfig describes a single RSS or Atom feed to poll.
type FeedConfig struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type rssFeed struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Title string    `xml:"title"`
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	Description string `xml:"description"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
}

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Title   string      `xml:"title"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	Title     string      `xml:"title"`
	ID        string      `xml:"id"`
	Link      atomLink    `xml:"link"`
	Summary   string      `xml:"summary"`
	Content   atomContent `xml:"content"`
	Published string      `xml:"published"`
	Updated   string      `xml:"updated"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

type atomContent struct {
	Type string `xml:"type,attr"`
	Body string `xml:",chardata"`
}

// feedEntry is the normalized representation of a feed item
// after parsing either RSS or Atom XML.
type feedEntry struct {
	ID      string
	Title   string
	Link    string
	Content string
	PubDate string
}
