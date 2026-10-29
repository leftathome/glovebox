package records

import (
	"strings"
	"testing"
	"time"
)

func TestConfigDisabledIsNotChecked(t *testing.T) {
	c := Config{} // enabled false, everything empty
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled config refused: %v", err)
	}
	c = Config{JournalURL: "ftp://x", TokenFile: "rel", TimeoutSeconds: 1}
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled config with junk refused: %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{Enabled: true, JournalURL: "http://glovebox-glovebox-journal:9095", TokenFile: "/var/run/secrets/glovebox-journal/token"}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 90*time.Second || c.Spool.MaxAge() != 7*24*time.Hour || c.Spool.MaxBytes != 256<<20 {
		t.Fatalf("defaults: %+v", c)
	}
	if c.Spool.MinBackoff() != 30*time.Second || c.Spool.MaxBackoff() != time.Hour {
		t.Fatalf("backoff defaults: %+v", c.Spool)
	}
}

func TestConfigAcceptsTrailingSlashAndHTTPS(t *testing.T) {
	for _, u := range []string{"http://j:9095/", "https://j.example:9095"} {
		c := Config{Enabled: true, JournalURL: u, TokenFile: "/t/token"}
		c.ApplyDefaults()
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

func TestConfigRefusals(t *testing.T) {
	ok := func() Config {
		c := Config{Enabled: true, JournalURL: "http://j:9095", TokenFile: "/t/token"}
		c.ApplyDefaults()
		return c
	}
	cases := map[string]func(*Config){
		"no url":          func(c *Config) { c.JournalURL = "" },
		"url with path":   func(c *Config) { c.JournalURL = "http://j:9095/v1" },
		"url with query":  func(c *Config) { c.JournalURL = "http://j:9095?a=1" },
		"url with frag":   func(c *Config) { c.JournalURL = "http://j:9095#x" },
		"url with user":   func(c *Config) { c.JournalURL = "http://u@j:9095" },
		"ftp url":         func(c *Config) { c.JournalURL = "ftp://j" },
		"no token":        func(c *Config) { c.TokenFile = "" },
		"relative token":  func(c *Config) { c.TokenFile = "token" },
		"token with nl":   func(c *Config) { c.TokenFile = "/t/to\nken" },
		"timeout low":     func(c *Config) { c.TimeoutSeconds = 5 },
		"timeout high":    func(c *Config) { c.TimeoutSeconds = 601 },
		"max age zero":    func(c *Config) { c.Spool.MaxAgeHours = -1 },
		"max age high":    func(c *Config) { c.Spool.MaxAgeHours = 24*90 + 1 },
		"max bytes small": func(c *Config) { c.Spool.MaxBytes = 1 << 10 },
		"max bytes big":   func(c *Config) { c.Spool.MaxBytes = 4<<30 + 1 },
		"backoff order":   func(c *Config) { c.Spool.MinBackoffSeconds, c.Spool.MaxBackoffSeconds = 600, 60 },
		"backoff low":     func(c *Config) { c.Spool.MinBackoffSeconds = -1 },
		"backoff high":    func(c *Config) { c.Spool.MaxBackoffSeconds = 86401 },
	}
	for name, mutate := range cases {
		c := ok()
		mutate(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "records") {
			t.Errorf("%s: err = %v, want a records error", name, err)
		}
	}
}

func TestConfigBoundsAreInclusive(t *testing.T) {
	c := Config{Enabled: true, JournalURL: "http://j:9095", TokenFile: "/t/token", TimeoutSeconds: 10,
		Spool: SpoolConfig{MaxAgeHours: 1, MaxBytes: 1 << 20, MinBackoffSeconds: 1, MaxBackoffSeconds: 1}}
	if err := c.Validate(); err != nil {
		t.Fatalf("lower bounds: %v", err)
	}
	c.TimeoutSeconds, c.Spool = 600, SpoolConfig{MaxAgeHours: 2160, MaxBytes: 4 << 30, MinBackoffSeconds: 86400, MaxBackoffSeconds: 86400}
	if err := c.Validate(); err != nil {
		t.Fatalf("upper bounds: %v", err)
	}
}
