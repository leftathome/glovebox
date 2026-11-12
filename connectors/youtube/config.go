package main

import (
	"fmt"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// Config is the full configuration for the YouTube connector.
type Config struct {
	connector.BaseConfig

	// Channels to follow. Each entry is a channel ID ("UC...") or a handle
	// ("@JeffGeerling"); handles are resolved once via channels.list and
	// cached in the checkpoint. The routing key for a channel is
	// "channel:<entry as written>", so a rule matches the same token the
	// operator configured (or use "*" for one rule covering every channel).
	Channels []string `json:"channels"`

	// ChannelIDs is the original name for Channels, still accepted. Entries
	// from both lists are followed.
	ChannelIDs []string `json:"channel_ids,omitempty"`

	// PollInterval is a Go duration (default "1h", minimum "5m").
	PollInterval string `json:"poll_interval,omitempty"`

	// InitialBackfill is how many of a channel's newest uploads to stage the
	// first time it is polled (no checkpoint yet). Default 3. Keeps a newly
	// followed channel from flooding caro with its last 50 uploads.
	InitialBackfill int `json:"initial_backfill,omitempty"`

	// MaxNewPerPoll caps how many new uploads one channel may stage per poll.
	// Default 10. When exceeded the newest are kept and the rest skipped.
	MaxNewPerPoll int `json:"max_new_per_poll,omitempty"`

	FetchComments *bool `json:"fetch_comments,omitempty"` // default true
	MaxComments   int   `json:"max_comments,omitempty"`   // default 25, max 100

	Transcript TranscriptConfig `json:"transcript,omitempty"`
}

// TranscriptConfig selects the transcript provider (glovebox-iwqm).
type TranscriptConfig struct {
	// Provider: "none" (default) or "yt-dlp" (captions only, never the
	// video; glovebox-9zmo).
	Provider string `json:"provider,omitempty"`
	// YtdlpPath is the yt-dlp executable for provider "yt-dlp". Default
	// "yt-dlp" (on PATH in the connector image).
	YtdlpPath string `json:"ytdlp_path,omitempty"`
	// Timeout per video for provider "yt-dlp", a Go duration. Default "90s".
	Timeout string `json:"timeout,omitempty"`
	// Languages in preference order. Default ["en"].
	Languages []string `json:"languages,omitempty"`
	// SkipCategories are videoCategory IDs for which no transcript is
	// attempted. Default ["10"] (Music): lyrics are not a useful transcript.
	SkipCategories []string `json:"skip_categories,omitempty"`
}

const (
	defaultPollInterval = time.Hour
	minPollInterval     = 5 * time.Minute
)

// EffectivePollInterval parses PollInterval, defaulting to 1h.
func (cfg Config) EffectivePollInterval() (time.Duration, error) {
	if cfg.PollInterval == "" {
		return defaultPollInterval, nil
	}
	d, err := time.ParseDuration(cfg.PollInterval)
	if err != nil {
		return 0, fmt.Errorf("poll_interval %q: %w", cfg.PollInterval, err)
	}
	if d < minPollInterval {
		return 0, fmt.Errorf("poll_interval %s is below the %s minimum", d, minPollInterval)
	}
	return d, nil
}

// AllChannels returns Channels followed by ChannelIDs, de-duplicated.
func (cfg Config) AllChannels() []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{cfg.Channels, cfg.ChannelIDs} {
		for _, c := range list {
			if c != "" && !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

func (cfg Config) EffectiveInitialBackfill() int {
	if cfg.InitialBackfill > 0 {
		return cfg.InitialBackfill
	}
	return 3
}

func (cfg Config) EffectiveMaxNewPerPoll() int {
	if cfg.MaxNewPerPoll > 0 {
		return cfg.MaxNewPerPoll
	}
	return 10
}

// ShouldFetchComments returns whether comments should be fetched (default true).
func (cfg Config) ShouldFetchComments() bool {
	if cfg.FetchComments == nil {
		return true
	}
	return *cfg.FetchComments
}

// EffectiveMaxComments returns the configured max comments or 25 as default.
// The API caps commentThreads maxResults at 100.
func (cfg Config) EffectiveMaxComments() int {
	switch {
	case cfg.MaxComments > 100:
		return 100
	case cfg.MaxComments > 0:
		return cfg.MaxComments
	}
	return 25
}

func (t TranscriptConfig) EffectiveLanguages() []string {
	if len(t.Languages) > 0 {
		return t.Languages
	}
	return []string{"en"}
}

func (t TranscriptConfig) EffectiveSkipCategories() []string {
	if t.SkipCategories != nil {
		return t.SkipCategories
	}
	return []string{"10"}
}

// newTranscriptProvider builds the configured provider.
func newTranscriptProvider(t TranscriptConfig) (TranscriptProvider, error) {
	switch t.Provider {
	case "", "none":
		return noTranscriptProvider{}, nil
	case "yt-dlp":
		p := ytdlpProvider{bin: t.YtdlpPath, timeout: 90 * time.Second}
		if p.bin == "" {
			p.bin = "yt-dlp"
		}
		if t.Timeout != "" {
			d, err := time.ParseDuration(t.Timeout)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("transcript.timeout %q is not a positive duration", t.Timeout)
			}
			p.timeout = d
		}
		return p, nil
	}
	return nil, fmt.Errorf("transcript.provider %q is not supported (supported: none, yt-dlp)", t.Provider)
}
