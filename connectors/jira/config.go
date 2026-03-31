package main

import (
	"github.com/leftathome/glovebox/connector"
)

// Config is the full configuration for the Jira connector.
type Config struct {
	connector.BaseConfig
	BaseURL  string   `json:"base_url"`  // e.g. "https://mycompany.atlassian.net"
	Projects []string `json:"projects"`  // project keys, e.g. ["PROJ", "OPS"]
}

// jiraSearchResponse is the top-level JSON response from the Jira search API.
type jiraSearchResponse struct {
	StartAt    int          `json:"startAt"`
	MaxResults int          `json:"maxResults"`
	Total      int          `json:"total"`
	Issues     []jiraIssue  `json:"issues"`
}

// jiraIssue represents a single issue in the Jira search response.
type jiraIssue struct {
	Key    string      `json:"key"`
	Fields jiraFields  `json:"fields"`
}

// jiraFields holds the fields we care about from a Jira issue.
type jiraFields struct {
	Summary     string      `json:"summary"`
	Status      jiraStatus  `json:"status"`
	Description interface{} `json:"description"` // ADF or string
	Updated     string      `json:"updated"`
}

// jiraStatus is the status object within a Jira issue.
type jiraStatus struct {
	Name string `json:"name"`
}

// issueSummary is the JSON content written to staging for each issue.
type issueSummary struct {
	Key         string `json:"key"`
	Summary     string `json:"summary"`
	Status      string `json:"status"`
	Description string `json:"description"`
}
