package powerschool

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

	Kids             []Kid             `json:"kids"`
	PollSchedule     PollSchedule      `json:"poll_schedule"`
	Surfaces         SurfacesConfig    `json:"surfaces"`
	Assignments      AssignmentsConfig `json:"assignments"`
	FailureThreshold int               `json:"failure_threshold"`
}

// Kid maps an operator-chosen opaque label to a PowerSchool student. Per
// spec 16 section 6: use opaque labels (k1, k2), never a name, so no PII
// lands in match keys, tags or audit logs.
//
// StudentID is the numeric id in the portal's student switcher
// (javascript:switchStudent(<id>)). It is a string because the library
// treats it as one and leading zeros are significant on some instances.
//
// StudentNumber is optional. When set it is the school-issued student
// number shown on the portal page ("Student ID #"), and a second,
// independent check on the selection: the selected student's number, parsed
// from the same page as the grades, must equal it or the kid is skipped for
// that poll. It is an identifier of a real child, so set it through the
// deployment's private values, never in a committed file.
type Kid struct {
	Name          string `json:"name"`
	StudentID     string `json:"student_id"`
	StudentNumber string `json:"student_number,omitempty"`
}

// PollSchedule defines when the connector polls. See spec 16 section 8.
type PollSchedule struct {
	WeekdaysOnly bool         `json:"weekdays_only"`
	Windows      []PollWindow `json:"windows"`
}

// PollWindow is one daily polling window. Times are local-time HH:MM
// strings interpreted in the connector's timezone (env POWERSCHOOL_TIMEZONE,
// default America/Los_Angeles).
type PollWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// SurfacesConfig switches individual surfaces off. A nil pointer means
// "not set" and ApplyDefaults turns it on, so an omitted key enables the
// surface and only an explicit false disables it.
type SurfacesConfig struct {
	Grades      *bool `json:"grades"`
	Assignments *bool `json:"assignments"`
	Attendance  *bool `json:"attendance"`
}

// AssignmentsConfig tunes the assignments surface.
type AssignmentsConfig struct {
	// LookbackDays bounds the first-sight backlog: an assignment due more
	// than this many days ago that is neither missing, late nor incomplete
	// is recorded silently instead of staged. A later change to it still
	// stages. See spec 16 section 7.2.
	LookbackDays int `json:"lookback_days"`
	// CourseDelayMS spaces the per-course requests so a poll does not hit
	// the portal with a burst.
	CourseDelayMS int `json:"course_delay_ms"`
}

// ApplyDefaults fills in zero-value fields with the spec 16 defaults. Does
// NOT override explicit non-zero values.
func ApplyDefaults(c *Config) {
	on := true
	if c.Surfaces.Grades == nil {
		c.Surfaces.Grades = &on
	}
	if c.Surfaces.Assignments == nil {
		c.Surfaces.Assignments = &on
	}
	if c.Surfaces.Attendance == nil {
		c.Surfaces.Attendance = &on
	}
	if c.Assignments.LookbackDays == 0 {
		c.Assignments.LookbackDays = 30
	}
	if c.Assignments.CourseDelayMS == 0 {
		c.Assignments.CourseDelayMS = 500
	}
	if c.FailureThreshold == 0 {
		c.FailureThreshold = 5
	}
}

// GradesEnabled reports whether the grades surface is on.
func (c *Config) GradesEnabled() bool { return c.Surfaces.Grades == nil || *c.Surfaces.Grades }

// AssignmentsEnabled reports whether the assignments surface is on. It also
// decides whether the transport's single POST exception is opened.
func (c *Config) AssignmentsEnabled() bool {
	return c.Surfaces.Assignments == nil || *c.Surfaces.Assignments
}

// AttendanceEnabled reports whether the attendance surface is on.
func (c *Config) AttendanceEnabled() bool {
	return c.Surfaces.Attendance == nil || *c.Surfaces.Attendance
}

// ValidateConfig enforces the spec 16 invariants. Run at startup before the
// connector begins polling. The framework's BaseConfig validation (rules,
// identity defaults) is applied separately by connector.Run.
func ValidateConfig(c *Config) error {
	if len(c.Kids) == 0 {
		return fmt.Errorf("kids: at least one kid required")
	}
	seenName := make(map[string]bool, len(c.Kids))
	seenID := make(map[string]bool, len(c.Kids))
	seenNumber := make(map[string]bool, len(c.Kids))
	for i, k := range c.Kids {
		if k.Name == "" {
			return fmt.Errorf("kids[%d]: empty name", i)
		}
		// The label is embedded in match keys and checkpoint keys, both
		// colon-delimited.
		if strings.ContainsAny(k.Name, ": \t\n") {
			return fmt.Errorf("kids[%d]: name %q must not contain ':' or whitespace", i, k.Name)
		}
		if !isStudentID(k.StudentID) {
			return fmt.Errorf("kids[%d] (%s): student_id must be a non-zero numeric string", i, k.Name)
		}
		if seenName[k.Name] {
			return fmt.Errorf("kids: duplicate name %q", k.Name)
		}
		// Two labels on one student would stage that child's records
		// under both subjects.
		if seenID[k.StudentID] {
			return fmt.Errorf("kids: duplicate student_id (kid %q)", k.Name)
		}
		if k.StudentNumber != "" {
			if strings.TrimSpace(k.StudentNumber) != k.StudentNumber || strings.ContainsAny(k.StudentNumber, " \t\n") {
				return fmt.Errorf("kids[%d] (%s): student_number must not contain whitespace", i, k.Name)
			}
			if seenNumber[k.StudentNumber] {
				return fmt.Errorf("kids: duplicate student_number (kid %q)", k.Name)
			}
			seenNumber[k.StudentNumber] = true
		}
		seenName[k.Name] = true
		seenID[k.StudentID] = true
	}
	if len(c.enabledSurfaces()) == 0 {
		return fmt.Errorf("surfaces: at least one surface must be enabled")
	}
	if err := validateKidRules(c); err != nil {
		return err
	}
	if c.Assignments.LookbackDays < 0 {
		return fmt.Errorf("assignments.lookback_days: must not be negative")
	}
	if c.Assignments.CourseDelayMS < 0 {
		return fmt.Errorf("assignments.course_delay_ms: must not be negative")
	}
	if c.FailureThreshold < 0 {
		return fmt.Errorf("failure_threshold: must not be negative")
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
	return nil
}

// enabledSurfaces lists the surfaces that are switched on.
func (c *Config) enabledSurfaces() []string {
	var out []string
	if c.GradesEnabled() {
		out = append(out, gradeSurface)
	}
	if c.AssignmentsEnabled() {
		out = append(out, assignmentSurface)
	}
	if c.AttendanceEnabled() {
		out = append(out, attendanceSurface)
	}
	return out
}

// validateKidRules is the per-child visibility check (spec 16 section 6).
// Every enabled surface of every kid must resolve to a rule that names a
// data_subject, one kid's surfaces must all name the same subject, and no
// two kids may share one. A catch-all "*" rule therefore cannot stand in
// for the per-kid rules: it would carry one subject for both children, or
// none.
func validateKidRules(c *Config) error {
	matcher := connector.NewRuleMatcher(c.Rules)
	owner := make(map[string]string, len(c.Kids))
	for _, k := range c.Kids {
		subject := ""
		for _, surface := range c.enabledSurfaces() {
			key := MatchKey(k.Name, surface)
			rule, ok := matcher.Match(key)
			if !ok {
				return fmt.Errorf("rules: no rule matches %q", key)
			}
			if rule.DataSubject == "" {
				return fmt.Errorf("rules: the rule matching %q must set data_subject", key)
			}
			if subject != "" && rule.DataSubject != subject {
				return fmt.Errorf("rules: kid %q resolves to more than one data_subject", k.Name)
			}
			subject = rule.DataSubject
		}
		if other, dup := owner[subject]; dup {
			return fmt.Errorf("rules: kids %q and %q resolve to the same data_subject", other, k.Name)
		}
		owner[subject] = k.Name
	}
	return nil
}

// isStudentID reports whether s is all digits and not all zeros. The chart
// ships "0" as the placeholder so an unedited install fails here.
func isStudentID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return strings.Trim(s, "0") != ""
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
