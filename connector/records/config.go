// Package records is the connector side of the record journal (design spec
// docs/superpowers/specs/2026-10-16-record-journal-design.md, 4.3, 4.14):
// typed field values the journal accepts, a durable spool of snapshots on
// the connector's state volume, and the sender that delivers them in order
// and acts on every answer by its reason. It knows nothing about any source.
//
// Off by default: with Config.Enabled false, New returns a nil *Emitter and
// nothing is created (no directory, metric, goroutine or log line). Every
// method of a nil *Emitter is a no-op.
package records

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// Config is a connector's "records" block. Nothing in it is read unless
// Enabled is true.
type Config struct {
	Enabled bool `json:"enabled"`
	// JournalURL is the journal listener, scheme and host only
	// ("http://glovebox-glovebox-journal:9095"). The chart sets it.
	JournalURL string `json:"journal_url"`
	// TokenFile is the projected ServiceAccount token for the audience
	// glovebox-journal. Re-read for every request; sent to JournalURL only.
	TokenFile string `json:"token_file"`
	// TimeoutSeconds bounds one request (default 90, 10-600). Above the
	// journal's own request timeout (default 60).
	TimeoutSeconds int         `json:"timeout_seconds"`
	Spool          SpoolConfig `json:"spool"`
}

// SpoolConfig bounds the spool (spec 4.14) and its retries.
type SpoolConfig struct {
	MaxAgeHours       int   `json:"max_age_hours"`       // default 168 (7 days), 1-2160
	MaxBytes          int64 `json:"max_bytes"`           // default 256 MiB, 1 MiB - 4 GiB
	MinBackoffSeconds int   `json:"min_backoff_seconds"` // default 30
	MaxBackoffSeconds int   `json:"max_backoff_seconds"` // default 3600, at most 86400
}

// Defaults.
const (
	DefaultTimeoutSeconds    = 90
	DefaultMaxAgeHours       = 168
	DefaultMaxBytes          = 256 << 20
	DefaultMinBackoffSeconds = 30
	DefaultMaxBackoffSeconds = 3600
)

// ApplyDefaults fills every zero value with its default.
func (c *Config) ApplyDefaults() {
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = DefaultTimeoutSeconds
	}
	if c.Spool.MaxAgeHours == 0 {
		c.Spool.MaxAgeHours = DefaultMaxAgeHours
	}
	if c.Spool.MaxBytes == 0 {
		c.Spool.MaxBytes = DefaultMaxBytes
	}
	if c.Spool.MinBackoffSeconds == 0 {
		c.Spool.MinBackoffSeconds = DefaultMinBackoffSeconds
	}
	if c.Spool.MaxBackoffSeconds == 0 {
		c.Spool.MaxBackoffSeconds = DefaultMaxBackoffSeconds
	}
}

// Timeout bounds one request to the journal.
func (c Config) Timeout() time.Duration { return time.Duration(c.TimeoutSeconds) * time.Second }

// MaxAge is how long any spool file is kept, sent or not.
func (s SpoolConfig) MaxAge() time.Duration { return time.Duration(s.MaxAgeHours) * time.Hour }

// MinBackoff is the first retry delay.
func (s SpoolConfig) MinBackoff() time.Duration {
	return time.Duration(s.MinBackoffSeconds) * time.Second
}

// MaxBackoff caps the retry delay.
func (s SpoolConfig) MaxBackoff() time.Duration {
	return time.Duration(s.MaxBackoffSeconds) * time.Second
}

// Validate checks an enabled block (after ApplyDefaults). A disabled block
// is never checked.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(c.JournalURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("records.journal_url must be an http or https URL with a host and nothing else")
	}
	if !strings.HasPrefix(c.TokenFile, "/") || strings.ContainsAny(c.TokenFile, " \t\r\n") {
		return errors.New("records.token_file must be the absolute path of the token projected for glovebox-journal")
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 600 {
		return errors.New("records.timeout_seconds must be between 10 and 600")
	}
	if c.Spool.MaxAgeHours < 1 || c.Spool.MaxAgeHours > 24*90 {
		return errors.New("records.spool.max_age_hours must be between 1 and 2160")
	}
	if c.Spool.MaxBytes < 1<<20 || c.Spool.MaxBytes > 4<<30 {
		return errors.New("records.spool.max_bytes must be between 1 MiB and 4 GiB")
	}
	if c.Spool.MinBackoffSeconds < 1 || c.Spool.MaxBackoffSeconds < c.Spool.MinBackoffSeconds || c.Spool.MaxBackoffSeconds > 86400 {
		return errors.New("records.spool: need 1 <= min_backoff_seconds <= max_backoff_seconds <= 86400")
	}
	return nil
}
