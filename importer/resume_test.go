package importer

import "testing"

func boolPtr(b bool) *bool { return &b }

// decideFakeManifest is a minimal Manifest used by the Decide tests. It
// carries just enough state to exercise the status / byte-offset branches.
type decideFakeManifest struct {
	status ManifestStatus
	offset int64
}

func (m *decideFakeManifest) Status() ManifestStatus { return m.status }
func (m *decideFakeManifest) ByteOffset() int64      { return m.offset }
func (m *decideFakeManifest) MessageIDs() []string   { return nil }

// TestDecide covers every rule in docs/specs/09-mbox-importer-design.md §3.1.1.
//
// A non-zero byte offset in the manifest's resume_state is the canonical
// "checkpoint exists" signal (see Decide doc comment).
//
// Base rules (no override):
//   - status == StatusComplete                   -> ExitComplete
//   - status == StatusInterrupted && offset > 0  -> Resume
//   - status == StatusInterrupted && offset == 0 -> StartFresh
//   - status == StatusFailed                     -> RequireExplicitResume
//   - status == StatusInProgress                 -> StartFresh
//   - manifest == nil                            -> StartFresh
//
// Override rules:
//   - resumeOverride == false                    -> always StartFresh (fresh restart)
//   - resumeOverride == true && offset > 0       -> Resume (even for failed)
//   - resumeOverride == true && offset == 0      -> StartFresh (nothing to resume from)
func TestDecide(t *testing.T) {
	tests := []struct {
		name       string
		manifest   Manifest
		override   *bool
		wantAction ResumeAction
	}{
		// --- Base rules, no override ---
		{
			name:       "complete exits immediately",
			manifest:   &decideFakeManifest{status: StatusComplete},
			override:   nil,
			wantAction: ExitComplete,
		},
		{
			name:       "complete with stale checkpoint still exits",
			manifest:   &decideFakeManifest{status: StatusComplete, offset: 100},
			override:   nil,
			wantAction: ExitComplete,
		},
		{
			name:       "interrupted with checkpoint resumes",
			manifest:   &decideFakeManifest{status: StatusInterrupted, offset: 12345},
			override:   nil,
			wantAction: Resume,
		},
		{
			name:       "interrupted without checkpoint starts fresh",
			manifest:   &decideFakeManifest{status: StatusInterrupted, offset: 0},
			override:   nil,
			wantAction: StartFresh,
		},
		{
			name:       "failed requires explicit resume",
			manifest:   &decideFakeManifest{status: StatusFailed, offset: 5000},
			override:   nil,
			wantAction: RequireExplicitResume,
		},
		{
			name:       "failed without checkpoint still requires explicit resume",
			manifest:   &decideFakeManifest{status: StatusFailed, offset: 0},
			override:   nil,
			wantAction: RequireExplicitResume,
		},
		{
			name:       "in_progress starts fresh (died before writing terminal status)",
			manifest:   &decideFakeManifest{status: StatusInProgress, offset: 2000},
			override:   nil,
			wantAction: StartFresh,
		},
		{
			name:       "missing manifest starts fresh",
			manifest:   nil,
			override:   nil,
			wantAction: StartFresh,
		},

		// --- Override == false forces fresh start regardless of state ---
		{
			name:       "override false forces fresh from complete",
			manifest:   &decideFakeManifest{status: StatusComplete},
			override:   boolPtr(false),
			wantAction: StartFresh,
		},
		{
			name:       "override false forces fresh from interrupted",
			manifest:   &decideFakeManifest{status: StatusInterrupted, offset: 12345},
			override:   boolPtr(false),
			wantAction: StartFresh,
		},
		{
			name:       "override false forces fresh from failed",
			manifest:   &decideFakeManifest{status: StatusFailed, offset: 12345},
			override:   boolPtr(false),
			wantAction: StartFresh,
		},

		// --- Override == true forces resume semantics ---
		{
			name:       "override true resumes failed",
			manifest:   &decideFakeManifest{status: StatusFailed, offset: 500},
			override:   boolPtr(true),
			wantAction: Resume,
		},
		{
			name:       "override true resumes interrupted",
			manifest:   &decideFakeManifest{status: StatusInterrupted, offset: 500},
			override:   boolPtr(true),
			wantAction: Resume,
		},
		{
			name:       "override true without checkpoint starts fresh",
			manifest:   &decideFakeManifest{status: StatusFailed, offset: 0},
			override:   boolPtr(true),
			wantAction: StartFresh,
		},
		{
			name:       "override true on complete still exits complete",
			manifest:   &decideFakeManifest{status: StatusComplete},
			override:   boolPtr(true),
			wantAction: ExitComplete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.manifest, tt.override)
			if got.Action != tt.wantAction {
				t.Fatalf("Decide(%+v, override=%v).Action = %v, want %v",
					tt.manifest, tt.override, got.Action, tt.wantAction)
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
