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

	AttendanceDays  AttendanceDaysConfig  `json:"attendance_days"`
	TermGrades      TermGradesConfig      `json:"term_grades"`
	TeacherComments TeacherCommentsConfig `json:"teacher_comments"`
}

// AttendanceDaysConfig tunes the attendance_day surface.
type AttendanceDaysConfig struct {
	// LookbackDays bounds the first-sight backlog: a coded day first seen
	// more than this many days after it happened is recorded silently.
	LookbackDays int `json:"lookback_days"`
	// Kinds maps the coarse `kind` tag to the attendance legend CATEGORIES
	// it covers (the part before the hyphen in the page's
	// "CODE=CATEGORY-Description"). Unset means defaultAttendanceKinds. A
	// category that appears under no kind is tagged kind=unknown and still
	// staged.
	Kinds map[string][]string `json:"kinds,omitempty"`
}

// TermGradesConfig tunes the term_grade surface.
type TermGradesConfig struct {
	// LookbackDays bounds the first-sight backlog: a stored grade first
	// seen more than this many days after its completion month is
	// recorded silently.
	LookbackDays int `json:"lookback_days"`
	// MinIntervalHours is the least time between reads of the grade
	// history page for one kid. Stored grades change a few times a year.
	MinIntervalHours int `json:"min_interval_hours"`
}

// TeacherCommentsConfig tunes the teacher_comment surface.
type TeacherCommentsConfig struct {
	// Terms lists the reporting terms to read (Q1, S1, ...). Empty means
	// every term the page's own selector offers.
	Terms []string `json:"terms"`
	// MinIntervalHours is the least time between reads of the comment
	// pages for one kid (one request per term).
	MinIntervalHours int `json:"min_interval_hours"`
	// RequireTermConfirmation, true unless set to false, stages comments
	// only from a page that itself marks which reporting term it shows.
	RequireTermConfirmation *bool `json:"require_term_confirmation"`
	// MaxTerms caps how many reporting terms are read per kid per poll
	// (one request each), whatever the page's selector offers.
	MaxTerms int `json:"max_terms"`
}

func (c TeacherCommentsConfig) requireTermConfirmation() bool {
	return c.RequireTermConfirmation == nil || *c.RequireTermConfirmation
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
//
// StudentAPIID is optional. It is the student id the portal embeds in this
// child's scores pages (powerschool-go's StudentAPIID), which is not the
// switcher id. Every scores page read for the kid must carry it (see
// binding.go). When it is unset the connector learns it on the kid's first
// clean poll and keeps it in the checkpoint; setting it here also protects
// that first poll. Like StudentNumber it identifies a real child.
type Kid struct {
	Name          string `json:"name"`
	StudentID     string `json:"student_id"`
	StudentNumber string `json:"student_number,omitempty"`
	StudentAPIID  string `json:"student_api_id,omitempty"`
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
//
// The three later surfaces are the other way round: each reads a further
// portal page per kid per poll, so each is off unless explicitly set true.
type SurfacesConfig struct {
	Grades      *bool `json:"grades"`
	Assignments *bool `json:"assignments"`
	Attendance  *bool `json:"attendance"`

	AttendanceDays  *bool `json:"attendance_days"`
	TermGrades      *bool `json:"term_grades"`
	TeacherComments *bool `json:"teacher_comments"`
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
	if c.AttendanceDays.LookbackDays == 0 {
		c.AttendanceDays.LookbackDays = 30
	}
	if c.AttendanceDays.Kinds == nil {
		c.AttendanceDays.Kinds = defaultAttendanceKinds()
	}
	if c.TermGrades.LookbackDays == 0 {
		c.TermGrades.LookbackDays = 365
	}
	if c.TermGrades.MinIntervalHours == 0 {
		c.TermGrades.MinIntervalHours = 24
	}
	if c.TeacherComments.MinIntervalHours == 0 {
		c.TeacherComments.MinIntervalHours = 24
	}
	if c.TeacherComments.MaxTerms == 0 {
		c.TeacherComments.MaxTerms = 12
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

// AttendanceDaysEnabled reports whether the attendance_day surface is on.
func (c *Config) AttendanceDaysEnabled() bool {
	return c.Surfaces.AttendanceDays != nil && *c.Surfaces.AttendanceDays
}

// TermGradesEnabled reports whether the term_grade surface is on.
func (c *Config) TermGradesEnabled() bool {
	return c.Surfaces.TermGrades != nil && *c.Surfaces.TermGrades
}

// TeacherCommentsEnabled reports whether the teacher_comment surface is on.
func (c *Config) TeacherCommentsEnabled() bool {
	return c.Surfaces.TeacherComments != nil && *c.Surfaces.TeacherComments
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
	seenAPIID := make(map[string]bool, len(c.Kids))
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
		if k.StudentAPIID != "" {
			if !isStudentID(k.StudentAPIID) {
				return fmt.Errorf("kids[%d] (%s): student_api_id must be a non-zero numeric string", i, k.Name)
			}
			// One id for two kids would let either kid's turn accept the
			// other child's pages.
			if seenAPIID[k.StudentAPIID] {
				return fmt.Errorf("kids: duplicate student_api_id (kid %q)", k.Name)
			}
			seenAPIID[k.StudentAPIID] = true
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
	if err := validateTeacherCommentRules(c); err != nil {
		return err
	}
	if c.AttendanceDays.LookbackDays < 0 {
		return fmt.Errorf("attendance_days.lookback_days: must not be negative")
	}
	owner := map[string]string{}
	for kind, categories := range c.AttendanceDays.Kinds {
		if strings.TrimSpace(kind) == "" || kind == attendanceKindUnknown || strings.ContainsAny(kind, " \t\n") {
			return fmt.Errorf("attendance_days.kinds: %q is not a usable kind name", kind)
		}
		for _, cat := range categories {
			key := strings.ToUpper(strings.TrimSpace(cat))
			if key == "" {
				return fmt.Errorf("attendance_days.kinds[%s]: empty category", kind)
			}
			if other, dup := owner[key]; dup && other != kind {
				return fmt.Errorf("attendance_days.kinds: category %q is listed under both %q and %q", cat, other, kind)
			}
			owner[key] = kind
		}
	}
	if c.TermGrades.LookbackDays < 0 || c.TermGrades.MinIntervalHours < 0 {
		return fmt.Errorf("term_grades: lookback_days and min_interval_hours must not be negative")
	}
	if c.TeacherComments.MinIntervalHours < 0 {
		return fmt.Errorf("teacher_comments.min_interval_hours: must not be negative")
	}
	if c.TeacherComments.MaxTerms < 0 {
		return fmt.Errorf("teacher_comments.max_terms: must not be negative")
	}
	seenTerm := map[string]bool{}
	for i, term := range c.TeacherComments.Terms {
		if !isTerm(term) {
			return fmt.Errorf("teacher_comments.terms[%d]: %q is not a reporting term (1-8 letters or digits)", i, term)
		}
		if seenTerm[strings.ToUpper(term)] {
			return fmt.Errorf("teacher_comments.terms: duplicate term %q", term)
		}
		seenTerm[strings.ToUpper(term)] = true
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
	if c.AttendanceDaysEnabled() {
		out = append(out, attendanceDaySurface)
	}
	if c.TermGradesEnabled() {
		out = append(out, termGradeSurface)
	}
	if c.TeacherCommentsEnabled() {
		out = append(out, teacherCommentSurface)
	}
	return out
}

// teacherCommentAudience is the one audience a teacher comment may have.
// Owner decision: teacher comments are for the parents only.
const teacherCommentAudience = "guardians"

// guardiansOnly reports whether audience is set and every token in it is
// exactly "guardians". It is an allowlist: "subject", "household",
// "public", "siblings", "caregivers", a token in another case and an
// unknown token all fail, alone or next to "guardians".
func guardiansOnly(audience []string) bool {
	if len(audience) == 0 {
		return false
	}
	for _, a := range audience {
		if a != teacherCommentAudience {
			return false
		}
	}
	return true
}

// ruleCanMatch reports whether a rule's match pattern can match key. The
// framework's RuleMatcher matches a rule when its pattern equals the key or
// is "*". Prefix patterns ("powerschool:*", "powerschool:k1:*") are not
// matched by it today; they are treated here as able to match so that a
// matcher that learns them cannot open a path around this check.
func ruleCanMatch(pattern, key string) bool {
	if pattern == key || pattern == "*" {
		return true
	}
	if prefix, isGlob := strings.CutSuffix(pattern, "*"); isGlob {
		return strings.HasPrefix(key, prefix)
	}
	return false
}

// validateTeacherCommentRules enforces the owner's decision that teacher
// comments are for the parents only, as an allowlist:
//
//   - EVERY rule that can match a kid's teacher_comment key -- the exact
//     rule, a kid wildcard, "powerschool:*" or "*" -- must set an audience
//     in which every token is exactly "guardians". It does not matter
//     where the rule sits or whether another rule would win: a rule that
//     could route a teacher comment anywhere else is a config error.
//   - An unset audience is refused: it would fall back to a default this
//     connector does not control.
//   - The RuleMatcher is first-match-wins. If a kid has an exact
//     teacher_comment rule, no broader rule may come before it, even a
//     guardians-only one: it would win, and the exact rule (with that
//     kid's data_subject) would never be used.
//
// It applies whether or not the surface is enabled.
func validateTeacherCommentRules(c *Config) error {
	for _, k := range c.Kids {
		key := MatchKey(k.Name, teacherCommentSurface)
		exactAt := -1
		for i, rule := range c.Rules {
			if rule.Match == key {
				exactAt = i
				break
			}
		}
		for i, rule := range c.Rules {
			if !ruleCanMatch(rule.Match, key) {
				continue
			}
			if !guardiansOnly(rule.Audience) {
				return fmt.Errorf("rules[%d] (match %q) can match %q and has audience %q; teacher comments are for "+
					"the parents only: the audience must be set and be exactly [\"guardians\"]", i, rule.Match, key, rule.Audience)
			}
			if exactAt >= 0 && i < exactAt {
				return fmt.Errorf("rules[%d] (match %q) comes before the rule for %q and would win over it; "+
					"move the teacher_comment rule first", i, rule.Match, key)
			}
		}
	}
	return nil
}

// isTerm reports whether s looks like a reporting term code.
func isTerm(s string) bool {
	if s == "" || len(s) > 8 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
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
