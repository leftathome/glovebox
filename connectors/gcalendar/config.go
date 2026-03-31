package main

import "github.com/leftathome/glovebox/connector"

// Config is the full configuration for the Google Calendar connector.
type Config struct {
	connector.BaseConfig
	CalendarIDs []string `json:"calendar_ids"` // default ["primary"]
}
