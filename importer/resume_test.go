package importer

import "testing"

func boolPtr(b bool) *bool { return &b }

// TestDecide covers every rule in docs/specs/09-mbox-importer-design.md §3.1.1.
//
// Base rules (no override):
//   - status == "complete"                       -> ExitComplete
//   - status == "interrupted" && checkpoint      -> Resume
//   - status == "interrupted" && !checkpoint     -> StartFresh (stale checkpoint ignored)
//   - status == "failed"                         -> RequireExplicitResume
//   - status == "in_progress"                    -> StartFresh
//   - status == "" (manifest absent)             -> StartFresh
//
// Override rules:
//   - resumeOverride == false                    -> always StartFresh (fresh restart)
//   - resumeOverride == true && checkpoint       -> Resume (even for failed)
//   - resumeOverride == true && !checkpoint      -> StartFresh (nothing to resume from)
func TestDecide(t *testing.T) {
	tests := []struct {
		name            string
		status          string
		checkpoint      bool
		override        *bool
		wantAction      ResumeAction
		wantDescription string
	}{
		// --- Base rules, no override ---
		{
			name:       "complete exits immediately",
			status:     "complete",
			checkpoint: false,
			override:   nil,
			wantAction: ExitComplete,
		},
		{
			name:       "complete with stale checkpoint still exits",
			status:     "complete",
			checkpoint: true,
			override:   nil,
			wantAction: ExitComplete,
		},
		{
			name:       "interrupted with checkpoint resumes",
			status:     "interrupted",
			checkpoint: true,
			override:   nil,
			wantAction: Resume,
		},
		{
			name:       "interrupted without checkpoint starts fresh",
			status:     "interrupted",
			checkpoint: false,
			override:   nil,
			wantAction: StartFresh,
		},
		{
			name:       "failed requires explicit resume",
			status:     "failed",
			checkpoint: true,
			override:   nil,
			wantAction: RequireExplicitResume,
		},
		{
			name:       "failed without checkpoint still requires explicit resume",
			status:     "failed",
			checkpoint: false,
			override:   nil,
			wantAction: RequireExplicitResume,
		},
		{
			name:       "in_progress starts fresh (died before writing terminal status)",
			status:     "in_progress",
			checkpoint: true,
			override:   nil,
			wantAction: StartFresh,
		},
		{
			name:       "missing manifest starts fresh",
			status:     "",
			checkpoint: false,
			override:   nil,
			wantAction: StartFresh,
		},
		{
			name:       "missing manifest with stale checkpoint starts fresh",
			status:     "",
			checkpoint: true,
			override:   nil,
			wantAction: StartFresh,
		},

		// --- Override == false forces fresh start regardless of state ---
		{
			name:       "override false forces fresh from complete",
			status:     "complete",
			checkpoint: false,
			override:   boolPtr(false),
			wantAction: StartFresh,
		},
		{
			name:       "override false forces fresh from interrupted",
			status:     "interrupted",
			checkpoint: true,
			override:   boolPtr(false),
			wantAction: StartFresh,
		},
		{
			name:       "override false forces fresh from failed",
			status:     "failed",
			checkpoint: true,
			override:   boolPtr(false),
			wantAction: StartFresh,
		},

		// --- Override == true forces resume semantics ---
		{
			name:       "override true resumes failed",
			status:     "failed",
			checkpoint: true,
			override:   boolPtr(true),
			wantAction: Resume,
		},
		{
			name:       "override true resumes interrupted",
			status:     "interrupted",
			checkpoint: true,
			override:   boolPtr(true),
			wantAction: Resume,
		},
		{
			name:       "override true without checkpoint starts fresh",
			status:     "failed",
			checkpoint: false,
			override:   boolPtr(true),
			wantAction: StartFresh,
		},
		{
			name:       "override true on complete still exits complete",
			status:     "complete",
			checkpoint: false,
			override:   boolPtr(true),
			wantAction: ExitComplete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.status, tt.checkpoint, tt.override)
			if got.Action != tt.wantAction {
				t.Fatalf("Decide(%q, checkpoint=%v, override=%v).Action = %v, want %v",
					tt.status, tt.checkpoint, tt.override, got.Action, tt.wantAction)
			}
		})
	}
}

func TestResumeActionString(t *testing.T) {
	// Each action has a distinct, human-readable String(). Missing values
	// should not collapse to the same label because operator logs read them.
	seen := map[string]bool{}
	for _, a := range []ResumeAction{StartFresh, Resume, ExitComplete, RequireExplicitResume} {
		s := a.String()
		if s == "" {
			t.Errorf("ResumeAction(%d).String() is empty", a)
		}
		if seen[s] {
			t.Errorf("duplicate ResumeAction.String() value %q", s)
		}
		seen[s] = true
	}
}
