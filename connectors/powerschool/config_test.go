package powerschool

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

// validConfig returns a two-kid config that passes ValidateConfig.
func validConfig() Config {
	cfg := Config{
		BaseConfig: connector.BaseConfig{Rules: testRules()},
		Kids: []Kid{
			{Name: "k1", StudentID: "111111"},
			{Name: "k2", StudentID: "111112"},
		},
		PollSchedule: PollSchedule{Windows: []PollWindow{{Start: "16:30", End: "18:00"}}},
	}
	ApplyDefaults(&cfg)
	return cfg
}

func boolPtr(b bool) *bool { return &b }

func TestApplyDefaults(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)
	if !cfg.GradesEnabled() || !cfg.AssignmentsEnabled() || !cfg.AttendanceEnabled() {
		t.Error("surfaces are not on by default")
	}
	if cfg.Assignments.LookbackDays != 30 || cfg.Assignments.CourseDelayMS != 500 || cfg.FailureThreshold != 5 {
		t.Errorf("defaults = %+v threshold %d", cfg.Assignments, cfg.FailureThreshold)
	}

	explicit := Config{
		Surfaces:         SurfacesConfig{Assignments: boolPtr(false)},
		Assignments:      AssignmentsConfig{LookbackDays: 7, CourseDelayMS: 50},
		FailureThreshold: 2,
	}
	ApplyDefaults(&explicit)
	if explicit.AssignmentsEnabled() {
		t.Error("explicit assignments=false was overridden")
	}
	if !explicit.GradesEnabled() {
		t.Error("unset grades surface was not defaulted on")
	}
	if explicit.Assignments.LookbackDays != 7 || explicit.Assignments.CourseDelayMS != 50 || explicit.FailureThreshold != 2 {
		t.Errorf("explicit values were overridden: %+v", explicit)
	}
}

func TestValidateConfig(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // substring; empty = valid
	}{
		{name: "valid", mutate: func(c *Config) {}},
		{name: "no kids", mutate: func(c *Config) { c.Kids = nil }, wantErr: "at least one kid"},
		{name: "empty kid name", mutate: func(c *Config) { c.Kids[0].Name = "" }, wantErr: "empty name"},
		{name: "colon in kid name", mutate: func(c *Config) { c.Kids[0].Name = "k:1" }, wantErr: "must not contain"},
		{name: "space in kid name", mutate: func(c *Config) { c.Kids[0].Name = "k 1" }, wantErr: "must not contain"},
		{name: "empty student id", mutate: func(c *Config) { c.Kids[0].StudentID = "" }, wantErr: "student_id"},
		{name: "placeholder student id", mutate: func(c *Config) { c.Kids[0].StudentID = "0" }, wantErr: "student_id"},
		{name: "non-numeric student id", mutate: func(c *Config) { c.Kids[0].StudentID = "12a" }, wantErr: "student_id"},
		{name: "duplicate kid name", mutate: func(c *Config) { c.Kids[1].Name = "k1" }, wantErr: "duplicate name"},
		{name: "duplicate student id", mutate: func(c *Config) { c.Kids[1].StudentID = "111111" }, wantErr: "duplicate student_id"},
		{
			name: "all surfaces off",
			mutate: func(c *Config) {
				c.Surfaces = SurfacesConfig{Grades: boolPtr(false), Assignments: boolPtr(false), Attendance: boolPtr(false)}
			},
			wantErr: "at least one surface",
		},
		{name: "negative lookback", mutate: func(c *Config) { c.Assignments.LookbackDays = -1 }, wantErr: "lookback_days"},
		{name: "negative course delay", mutate: func(c *Config) { c.Assignments.CourseDelayMS = -1 }, wantErr: "course_delay_ms"},
		{name: "negative failure threshold", mutate: func(c *Config) { c.FailureThreshold = -1 }, wantErr: "failure_threshold"},
		{name: "no windows", mutate: func(c *Config) { c.PollSchedule.Windows = nil }, wantErr: "at least one window"},
		{name: "bad window start", mutate: func(c *Config) { c.PollSchedule.Windows[0].Start = "25:00" }, wantErr: "windows[0].start"},
		{name: "bad window end", mutate: func(c *Config) { c.PollSchedule.Windows[0].End = "noon" }, wantErr: "windows[0].end"},
		{name: "window end before start", mutate: func(c *Config) { c.PollSchedule.Windows[0].End = "16:00" }, wantErr: "strictly after"},

		// Per-child visibility (validateKidRules).
		{name: "no rules at all", mutate: func(c *Config) { c.Rules = nil }, wantErr: "no rule matches"},
		{
			name:    "a kid's surface has no rule",
			mutate:  func(c *Config) { c.Rules = c.Rules[:5] },
			wantErr: `no rule matches "powerschool:k2:attendance"`,
		},
		{
			name:    "rule without data_subject",
			mutate:  func(c *Config) { c.Rules[1].DataSubject = "" },
			wantErr: "must set data_subject",
		},
		{
			name: "wildcard without data_subject cannot stand in",
			mutate: func(c *Config) {
				c.Rules = []connector.Rule{{Match: "*", Destination: "school"}}
			},
			wantErr: "must set data_subject",
		},
		{
			name: "wildcard with one data_subject cannot cover two kids",
			mutate: func(c *Config) {
				c.Rules = []connector.Rule{{Match: "*", Destination: "school", DataSubject: "e_333333"}}
			},
			wantErr: "same data_subject",
		},
		{
			name: "two kids on one data_subject",
			mutate: func(c *Config) {
				c.Rules[3].DataSubject, c.Rules[4].DataSubject, c.Rules[5].DataSubject = "e_333333", "e_333333", "e_333333"
			},
			wantErr: "same data_subject",
		},
		{
			name:    "one kid split across two data_subjects",
			mutate:  func(c *Config) { c.Rules[2].DataSubject = "e_444444" },
			wantErr: "more than one data_subject",
		},
		{
			name: "an earlier wildcard shadows the per-kid rules",
			mutate: func(c *Config) {
				c.Rules = append([]connector.Rule{{Match: "*", Destination: "inbox"}}, c.Rules...)
			},
			wantErr: "must set data_subject",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			tc.mutate(&cfg)
			err := ValidateConfig(&cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateConfig: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateConfig_DisabledSurfaceNeedsNoRule: with attendance off, the
// attendance rules can be dropped entirely.
func TestValidateConfig_DisabledSurfaceNeedsNoRule(t *testing.T) {
	cfg := validConfig()
	cfg.Surfaces.Attendance = boolPtr(false)
	var kept []connector.Rule
	for _, r := range cfg.Rules {
		if !strings.HasSuffix(r.Match, ":attendance") {
			kept = append(kept, r)
		}
	}
	cfg.Rules = kept
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
}

// TestSampleConfig keeps the shipped config.json loadable and valid, and
// checks it carries no "*" rule (which would defeat nothing here, but
// would invite an operator to rely on it).
func TestSampleConfig(t *testing.T) {
	data, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	ApplyDefaults(&cfg)
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("ValidateConfig(config.json): %v", err)
	}
	if err := connector.ValidateBaseConfig(&cfg.BaseConfig); err != nil {
		t.Fatalf("ValidateBaseConfig(config.json): %v", err)
	}
	for _, r := range cfg.Rules {
		if r.Match == "*" {
			t.Error("sample config has a wildcard rule")
		}
	}
	if len(cfg.Kids) != 2 || cfg.Kids[0].Name != "k1" {
		t.Errorf("kids = %+v", cfg.Kids)
	}
}
