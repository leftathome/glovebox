package schoology

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/leftathome/glovebox/connector"
)

// Config holds the connector's runtime config. Embeds connector.BaseConfig
// for the standard rules/identity/fetch_limits inherited from the framework.
type Config struct {
	connector.BaseConfig

	Kids                  []Kid             `json:"kids"`
	PollSchedule          PollSchedule      `json:"poll_schedule"`
	Trigger               TriggerConfig     `json:"trigger"`
	Attachments           AttachmentsConfig `json:"attachments"`
	ParseFailureThreshold int               `json:"parse_failure_threshold"`
}

// Kid maps an operator-chosen opaque label to a Schoology UID. Per spec 12
// §10: prefer opaque labels (k1, k2) over family nicknames or legal names
// to avoid placing PII in metadata and audit logs.
type Kid struct {
	Name         string `json:"name"`
	SchoologyUID int64  `json:"schoology_uid"`
}

// PollSchedule defines when the connector polls. See spec 12 §6.
type PollSchedule struct {
	WeekdaysOnly bool         `json:"weekdays_only"`
	Windows      []PollWindow `json:"windows"`
}

// PollWindow is one daily polling window. Times are local-time HH:MM
// strings interpreted in the connector's timezone (env SCHOOLOGY_TIMEZONE,
// default America/Los_Angeles per spec 12 §6.1).
type PollWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// TriggerConfig configures the HTTP trigger endpoint.
type TriggerConfig struct {
	DebounceSeconds int `json:"debounce_seconds"`
	ListenPort      int `json:"listen_port"`
}

// AttachmentsConfig caps per-attachment download size.
type AttachmentsConfig struct {
	MaxSizeMB int `json:"max_size_mb"`
}

// ApplyDefaults fills in zero-value fields with sensible defaults per
// spec 12 §4.2. Does NOT override explicit non-zero values.
func ApplyDefaults(c *Config) {
	if c.Trigger.DebounceSeconds == 0 {
		c.Trigger.DebounceSeconds = 60
	}
	if c.Trigger.ListenPort == 0 {
		c.Trigger.ListenPort = 8081
	}
	if c.Attachments.MaxSizeMB == 0 {
		c.Attachments.MaxSizeMB = 25
	}
	if c.ParseFailureThreshold == 0 {
		c.ParseFailureThreshold = 10
	}
}

// ValidateConfig enforces spec 12 §4 invariants. Run at startup before the
// connector begins polling. The framework's BaseConfig validation (rules,
// identity defaults) is applied separately by connector.Run.
func ValidateConfig(c *Config) error {
	if len(c.Kids) == 0 {
		return fmt.Errorf("kids: at least one kid required")
	}
	seen := make(map[string]bool, len(c.Kids))
	for i, k := range c.Kids {
		if k.Name == "" {
			return fmt.Errorf("kids[%d]: empty name", i)
		}
		if k.SchoologyUID == 0 {
			return fmt.Errorf("kids[%d] (%s): missing schoology_uid", i, k.Name)
		}
		if seen[k.Name] {
			return fmt.Errorf("kids: duplicate name %q", k.Name)
		}
		seen[k.Name] = true
	}
	for i, w := range c.PollSchedule.Windows {
		if err := validateTimeOfDay(w.Start); err != nil {
			return fmt.Errorf("poll_schedule.windows[%d].start: %w", i, err)
		}
		if err := validateTimeOfDay(w.End); err != nil {
			return fmt.Errorf("poll_schedule.windows[%d].end: %w", i, err)
		}
	}
	return nil
}

func validateTimeOfDay(s string) error {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return fmt.Errorf("expected HH:MM, got %q", s)
	}
	hh, err := strconv.Atoi(parts[0])
	if err != nil || hh < 0 || hh > 23 {
		return fmt.Errorf("invalid hour in %q", s)
	}
	mm, err := strconv.Atoi(parts[1])
	if err != nil || mm < 0 || mm > 59 {
		return fmt.Errorf("invalid minute in %q", s)
	}
	return nil
}
