package schoology

import (
	"fmt"
	"strconv"
	"strings"
	"time"

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

	// AssignmentDetail configures the assignment_detail surface. It is off
	// unless at least one kid is listed.
	AssignmentDetail AssignmentDetailConfig `json:"assignment_detail"`
}

// AssignmentDetailConfig configures the assignment_detail surface: one
// extra page read per assignment, staged as its own item. See
// assignment_detail.go. A zero numeric field means "use the default".
type AssignmentDetailConfig struct {
	// Kids lists the kid labels the surface is enabled for. Empty (the
	// default) disables it: no detail page is ever requested.
	Kids []string `json:"kids"`

	// MaxPerPoll caps how many assignments have their detail fetched in
	// one poll, across all kids (default 20). An assessment costs two
	// requests (the page and its comments page), so the request ceiling is
	// twice this.
	MaxPerPoll int `json:"max_per_poll"`

	// RefreshHours is how long a still-listed, unchanged assignment waits
	// before its detail is read again (default 24). That re-read is how a
	// teacher comment added later is noticed.
	RefreshHours int `json:"refresh_hours"`

	// RequestSpacingMS is the pause between two detail requests (default
	// 1500).
	RequestSpacingMS int `json:"request_spacing_ms"`
}

// Defaults and bounds for AssignmentDetailConfig.
const (
	defaultDetailMaxPerPoll       = 20
	maxDetailMaxPerPoll           = 200
	defaultDetailRefreshHours     = 24
	defaultDetailRequestSpacingMS = 1500
	minDetailRequestSpacingMS     = 1000
	maxDetailRequestSpacingMS     = 60000
)

// Enabled reports whether the surface is on for the named kid.
func (a AssignmentDetailConfig) Enabled(kid string) bool {
	for _, k := range a.Kids {
		if k == kid {
			return true
		}
	}
	return false
}

// maxPerPoll, refreshInterval and requestSpacing read the numeric fields
// with their defaults applied, so a Config that skipped ApplyDefaults (as
// tests build them) still behaves.
func (a AssignmentDetailConfig) maxPerPoll() int {
	if a.MaxPerPoll <= 0 {
		return defaultDetailMaxPerPoll
	}
	return a.MaxPerPoll
}

func (a AssignmentDetailConfig) refreshInterval() time.Duration {
	if a.RefreshHours <= 0 {
		return defaultDetailRefreshHours * time.Hour
	}
	return time.Duration(a.RefreshHours) * time.Hour
}

func (a AssignmentDetailConfig) requestSpacing() time.Duration {
	if a.RequestSpacingMS <= 0 {
		return defaultDetailRequestSpacingMS * time.Millisecond
	}
	return time.Duration(a.RequestSpacingMS) * time.Millisecond
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
	if c.AssignmentDetail.MaxPerPoll == 0 {
		c.AssignmentDetail.MaxPerPoll = defaultDetailMaxPerPoll
	}
	if c.AssignmentDetail.RefreshHours == 0 {
		c.AssignmentDetail.RefreshHours = defaultDetailRefreshHours
	}
	if c.AssignmentDetail.RequestSpacingMS == 0 {
		c.AssignmentDetail.RequestSpacingMS = defaultDetailRequestSpacingMS
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
	if len(c.PollSchedule.Windows) == 0 {
		return fmt.Errorf("poll_schedule.windows: at least one window required")
	}
	for i, w := range c.PollSchedule.Windows {
		startMin, err := parseTimeOfDay(w.Start)
		if err != nil {
			return fmt.Errorf("poll_schedule.windows[%d].start: %w", i, err)
		}
		endMin, err := parseTimeOfDay(w.End)
		if err != nil {
			return fmt.Errorf("poll_schedule.windows[%d].end: %w", i, err)
		}
		if endMin <= startMin {
			return fmt.Errorf("poll_schedule.windows[%d]: end %q must be strictly after start %q", i, w.End, w.Start)
		}
	}
	return validateAssignmentDetail(c, seen)
}

// validateAssignmentDetail checks the assignment_detail block. The numeric
// bounds apply even when the surface is off, so a typo is caught before the
// day someone enables it; zero means "default" and is accepted.
//
// An enabled kid must have a rule that names its key exactly, placed
// before any "*" rule. The rule matcher is first-match-wins and falls back
// to the wildcard, and a detail item routed by the wildcard would carry the
// config-wide default subject instead of the child's. Refusing to start is
// better than staging a child's assignment text under the wrong subject.
func validateAssignmentDetail(c *Config, kidNames map[string]bool) error {
	d := c.AssignmentDetail
	if d.MaxPerPoll < 0 || d.MaxPerPoll > maxDetailMaxPerPoll {
		return fmt.Errorf("assignment_detail.max_per_poll: %d out of range 1..%d", d.MaxPerPoll, maxDetailMaxPerPoll)
	}
	if d.RefreshHours < 0 {
		return fmt.Errorf("assignment_detail.refresh_hours: %d must be at least 1", d.RefreshHours)
	}
	if d.RequestSpacingMS != 0 &&
		(d.RequestSpacingMS < minDetailRequestSpacingMS || d.RequestSpacingMS > maxDetailRequestSpacingMS) {
		return fmt.Errorf("assignment_detail.request_spacing_ms: %d out of range %d..%d",
			d.RequestSpacingMS, minDetailRequestSpacingMS, maxDetailRequestSpacingMS)
	}
	enabled := make(map[string]bool, len(d.Kids))
	for i, name := range d.Kids {
		if !kidNames[name] {
			return fmt.Errorf("assignment_detail.kids[%d]: %q is not a configured kid", i, name)
		}
		if enabled[name] {
			return fmt.Errorf("assignment_detail.kids: duplicate name %q", name)
		}
		enabled[name] = true
		key := detailMatchKey(name)
		if !firstMatchIsExact(c.Rules, key) {
			return fmt.Errorf("assignment_detail.kids[%d]: no rule matches %q exactly ahead of any \"*\" rule", i, key)
		}
	}
	return nil
}

// firstMatchIsExact reports whether the rule the matcher would pick for key
// names key itself rather than the "*" wildcard.
func firstMatchIsExact(rules []connector.Rule, key string) bool {
	for _, r := range rules {
		if r.Match == key {
			return true
		}
		if r.Match == "*" {
			return false
		}
	}
	return false
}

// parseTimeOfDay parses an HH:MM local-time string into minutes-of-day
// (0..1439). Returns an error if the format or numeric ranges are invalid.
func parseTimeOfDay(s string) (int, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	hh, err := strconv.Atoi(parts[0])
	if err != nil || hh < 0 || hh > 23 {
		return 0, fmt.Errorf("invalid hour in %q", s)
	}
	mm, err := strconv.Atoi(parts[1])
	if err != nil || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("invalid minute in %q", s)
	}
	return hh*60 + mm, nil
}
