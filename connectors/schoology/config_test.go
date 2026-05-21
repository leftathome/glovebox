package schoology

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLoadConfig_Valid(t *testing.T) {
	j := `{
		"kids": [
			{"name": "k1", "schoology_uid": 12345678},
			{"name": "k2", "schoology_uid": 12345679}
		],
		"poll_schedule": {
			"weekdays_only": true,
			"windows": [
				{"start": "07:00", "end": "09:00"},
				{"start": "15:30", "end": "17:30"}
			]
		},
		"trigger": {
			"debounce_seconds": 60,
			"listen_port": 8081
		},
		"attachments": {
			"max_size_mb": 25
		},
		"parse_failure_threshold": 10,
		"rules": [
			{"match": "schoology:k1:assignment", "data_subject": "k1", "audience": ["household"], "destination": "school"},
			{"match": "schoology:message",                                "audience": ["guardians"], "destination": "school"}
		],
		"identity": {
			"provider": "schoology",
			"auth_method": "session_cookie",
			"tenant": "wagner-home"
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(j), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(cfg.Kids) != 2 || cfg.Kids[0].Name != "k1" {
		t.Errorf("kids: got %+v", cfg.Kids)
	}
}

func TestValidateConfig_DuplicateKidNames(t *testing.T) {
	cfg := Config{
		Kids: []Kid{
			{Name: "k1", SchoologyUID: 1},
			{Name: "k1", SchoologyUID: 2},
		},
	}
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-name error, got %v", err)
	}
}

func TestValidateConfig_EmptyKidName(t *testing.T) {
	cfg := Config{
		Kids: []Kid{{Name: "", SchoologyUID: 1}},
	}
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-name error, got %v", err)
	}
}

func TestValidateConfig_MissingSchoologyUID(t *testing.T) {
	cfg := Config{
		Kids: []Kid{{Name: "k1", SchoologyUID: 0}},
	}
	if err := ValidateConfig(&cfg); err == nil {
		t.Fatalf("expected missing-uid error")
	}
}

func TestValidateConfig_BadWindowTime(t *testing.T) {
	cfg := Config{
		Kids: []Kid{{Name: "k1", SchoologyUID: 1}},
		PollSchedule: PollSchedule{
			Windows: []PollWindow{{Start: "25:00", End: "26:00"}},
		},
	}
	if err := ValidateConfig(&cfg); err == nil {
		t.Fatalf("expected bad-window error")
	}
}

func TestValidateConfig_NoKids(t *testing.T) {
	cfg := Config{}
	if err := ValidateConfig(&cfg); err == nil {
		t.Fatalf("expected at-least-one-kid error")
	}
}

func TestValidateConfig_NoWindows(t *testing.T) {
	cfg := Config{
		Kids:         []Kid{{Name: "k1", SchoologyUID: 1}},
		PollSchedule: PollSchedule{Windows: nil},
	}
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "at least one window") {
		t.Fatalf("expected at-least-one-window error, got %v", err)
	}
}

func TestValidateConfig_WindowEndNotAfterStart(t *testing.T) {
	cases := []struct {
		name  string
		start string
		end   string
	}{
		{"backwards", "09:00", "07:00"},
		{"equal", "08:30", "08:30"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Kids: []Kid{{Name: "k1", SchoologyUID: 1}},
				PollSchedule: PollSchedule{
					Windows: []PollWindow{{Start: tc.start, End: tc.end}},
				},
			}
			err := ValidateConfig(&cfg)
			if err == nil || !strings.Contains(err.Error(), "must be strictly after") {
				t.Fatalf("expected strictly-after error, got %v", err)
			}
		})
	}
}

func TestApplyDefaults(t *testing.T) {
	cfg := Config{
		Kids: []Kid{{Name: "k1", SchoologyUID: 1}},
	}
	ApplyDefaults(&cfg)
	if cfg.Trigger.DebounceSeconds == 0 {
		t.Errorf("default DebounceSeconds not applied")
	}
	if cfg.Trigger.ListenPort == 0 {
		t.Errorf("default ListenPort not applied")
	}
	if cfg.Attachments.MaxSizeMB == 0 {
		t.Errorf("default MaxSizeMB not applied")
	}
	if cfg.ParseFailureThreshold == 0 {
		t.Errorf("default ParseFailureThreshold not applied")
	}
}

func TestApplyDefaults_DoesNotOverrideExplicit(t *testing.T) {
	cfg := Config{
		Kids:                  []Kid{{Name: "k1", SchoologyUID: 1}},
		Trigger:               TriggerConfig{DebounceSeconds: 30, ListenPort: 9999},
		Attachments:           AttachmentsConfig{MaxSizeMB: 50},
		ParseFailureThreshold: 5,
	}
	ApplyDefaults(&cfg)
	if cfg.Trigger.DebounceSeconds != 30 {
		t.Errorf("explicit DebounceSeconds overridden")
	}
	if cfg.Trigger.ListenPort != 9999 {
		t.Errorf("explicit ListenPort overridden")
	}
	if cfg.Attachments.MaxSizeMB != 50 {
		t.Errorf("explicit MaxSizeMB overridden")
	}
	if cfg.ParseFailureThreshold != 5 {
		t.Errorf("explicit threshold overridden")
	}
}
