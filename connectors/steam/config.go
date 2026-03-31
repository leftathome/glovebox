package main

import (
	"github.com/leftathome/glovebox/connector"
)

// Config is the full configuration for the Steam connector.
type Config struct {
	connector.BaseConfig
	Apps         []AppConfig `json:"apps"`
	FetchReviews bool        `json:"fetch_reviews"`
	FetchNews    bool        `json:"fetch_news"`
}

// AppConfig describes a single Steam application to monitor.
type AppConfig struct {
	ID   string `json:"id"`   // Steam appid
	Name string `json:"name"` // display name
}

// reviewsResponse is the top-level JSON envelope from the Steam reviews API.
type reviewsResponse struct {
	Success      int      `json:"success"`
	QuerySummary struct{} `json:"query_summary"`
	Reviews      []review `json:"reviews"`
}

// review is a single review from the Steam reviews API.
type review struct {
	RecommendationID string `json:"recommendationid"`
	Author           struct {
		SteamID string `json:"steamid"`
	} `json:"author"`
	Review          string `json:"review"`
	TimestampCreated int64  `json:"timestamp_created"`
	VotedUp         bool   `json:"voted_up"`
}

// newsResponse is the top-level JSON envelope from the Steam news API.
type newsResponse struct {
	AppNews struct {
		AppID    int        `json:"appid"`
		NewsItems []newsItem `json:"newsitems"`
	} `json:"appnews"`
}

// newsItem is a single news article from the Steam news API.
type newsItem struct {
	GID           string `json:"gid"`
	Title         string `json:"title"`
	URL           string `json:"url"`
	Contents      string `json:"contents"`
	Author        string `json:"author"`
	Date          int64  `json:"date"`
	FeedLabel     string `json:"feedlabel"`
	FeedName      string `json:"feed_name"`
}
