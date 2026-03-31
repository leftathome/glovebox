package main

import (
	"github.com/leftathome/glovebox/connector"
)

// Config is the full configuration for the Hacker News connector.
type Config struct {
	connector.BaseConfig
	Feeds          []string `json:"feeds"`           // "top", "new", "best", "ask", "show"
	FollowComments bool     `json:"follow_comments"`
	MaxComments    int      `json:"max_comments"` // default 10
}

// hnStory represents a Hacker News story item from the API.
type hnStory struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Text        string `json:"text"`
	Score       int    `json:"score"`
	By          string `json:"by"`
	Time        int64  `json:"time"`
	Descendants int    `json:"descendants"`
	Kids        []int  `json:"kids"`
	Type        string `json:"type"`
}

// hnComment represents a Hacker News comment item from the API.
type hnComment struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	By     string `json:"by"`
	Time   int64  `json:"time"`
	Kids   []int  `json:"kids"`
	Type   string `json:"type"`
	Parent int    `json:"parent"`
}

// storyContent is the JSON structure staged for each story.
type storyContent struct {
	Title    string   `json:"title"`
	URL      string   `json:"url"`
	Text     string   `json:"text"`
	Score    int      `json:"score"`
	Comments []string `json:"comments"`
}
