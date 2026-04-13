package importer

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

// --- Test doubles ------------------------------------------------------------

// fakeSurvey is a minimal SurveyFile for tests. stale reports whether
// the survey should claim staleness.
type fakeSurvey struct {
	id    string
	stale bool
}

func (s *fakeSurvey) IsStale(sourcePath string) (bool, error) {
	return s.stale, nil
}

type fakeManifest struct {
	status string
	offset int64
	ids    []string
}

func (m *fakeManifest) Status() string       { return m.status }
func (m *fakeManifest) ByteOffset() int64    { return m.offset }
func (m *fakeManifest) MessageIDs() []string { return m.ids }

// mockImporter records every orchestration callback so tests can
// assert RunOneShot drove the right sequence. Each Fn field can be
// overridden to inject errors or custom behavior.
type mockImporter struct {
	// canned returns
	existingSurvey   SurveyFile
	existingManifest Manifest
	checkpointExists bool
	filterCfg        FilterConfig

	// error injection
	surveyErr     error
	loadSurveyErr error
	manifestErr   error
	filterErr     error
	clearErr      error
	importErr     error

	// the survey object produced by Survey()
	surveyReturn SurveyFile

	// call counters
	surveyCalls   int32
	loadSurvey    int32
	loadManifest  int32
	ckptChecks    int32
	loadFilter    int32
	clearState    int32
	importCalls   int32
	importDecided ResumeDecision
	importSurvey  SurveyFile
	importFilter  FilterConfig
}

func (m *mockImporter) Survey(ctx context.Context, path string) (SurveyFile, error) {
	atomic.AddInt32(&m.surveyCalls, 1)
	if m.surveyErr != nil {
		return nil, m.surveyErr
	}
	if m.surveyReturn != nil {
		return m.surveyReturn, nil
	}
	return &fakeSurvey{id: "generated"}, nil
}

func (m *mockImporter) LoadSurvey(path string) (SurveyFile, error) {
	atomic.AddInt32(&m.loadSurvey, 1)
	if m.loadSurveyErr != nil {
		return nil, m.loadSurveyErr
	}
	return m.existingSurvey, nil
}

func (m *mockImporter) LoadManifest(path string) (Manifest, error) {
	atomic.AddInt32(&m.loadManifest, 1)
	if m.manifestErr != nil {
		return nil, m.manifestErr
	}
	return m.existingManifest, nil
}

func (m *mockImporter) CheckpointExists(path string) bool {
	atomic.AddInt32(&m.ckptChecks, 1)
	return m.checkpointExists
}

func (m *mockImporter) LoadFilter(filterPath string) (FilterConfig, error) {
	atomic.AddInt32(&m.loadFilter, 1)
	if m.filterErr != nil {
		return nil, m.filterErr
	}
	return m.filterCfg, nil
}

func (m *mockImporter) ClearState(path string) error {
	atomic.AddInt32(&m.clearState, 1)
	return m.clearErr
}

func (m *mockImporter) Import(ctx context.Context, path string, survey SurveyFile, filter FilterConfig, decision ResumeDecision) error {
	atomic.AddInt32(&m.importCalls, 1)
	m.importDecided = decision
	m.importSurvey = survey
	m.importFilter = filter
	return m.importErr
}

// testFramework returns a Framework value sufficient for RunOneShot's
// current needs. RunOneShot only reads fw.Logger; this keeps tests
// independent of the backend/metrics/health-server bootstrap.
func testFramework() *connector.Framework {
	return &connector.Framework{
		Name:   "test-importer",
		Logger: slog.Default(),
	}
}

// --- Fresh-start scenarios ---------------------------------------------------

func TestRunOneShot_FreshStartGeneratesSurveyAndImports(t *testing.T) {
	m := &mockImporter{}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if m.surveyCalls != 1 {
		t.Errorf("Survey called %d times, want 1", m.surveyCalls)
	}
	if m.importCalls != 1 {
		t.Errorf("Import called %d times, want 1", m.importCalls)
	}
	if m.importDecided.Action != StartFresh {
		t.Errorf("Import decision = %v, want StartFresh", m.importDecided.Action)
	}
}

func TestRunOneShot_UsesExistingFreshSurvey(t *testing.T) {
	existing := &fakeSurvey{id: "existing", stale: false}
	m := &mockImporter{existingSurvey: existing}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if m.surveyCalls != 0 {
		t.Errorf("Survey regenerated unexpectedly (calls=%d)", m.surveyCalls)
	}
	if m.importCalls != 1 {
		t.Errorf("Import not called")
	}
	if m.importSurvey != existing {
		t.Errorf("Import received wrong survey object")
	}
}

func TestRunOneShot_StaleSurveyIsRegenerated(t *testing.T) {
	existing := &fakeSurvey{id: "existing", stale: true}
	regenerated := &fakeSurvey{id: "regenerated", stale: false}
	m := &mockImporter{existingSurvey: existing, surveyReturn: regenerated}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if m.surveyCalls != 1 {
		t.Errorf("Survey should regenerate for stale survey (calls=%d)", m.surveyCalls)
	}
	if m.importSurvey != regenerated {
		t.Errorf("Import should receive regenerated survey")
	}
}

func TestRunOneShot_RegenerateSurveyForcesResurvey(t *testing.T) {
	existing := &fakeSurvey{id: "existing", stale: false} // fresh!
	regenerated := &fakeSurvey{id: "regenerated"}
	m := &mockImporter{existingSurvey: existing, surveyReturn: regenerated}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath:       "/tmp/archive.mbox",
		RegenerateSurvey: true,
	})
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if m.surveyCalls != 1 {
		t.Errorf("Survey should regenerate when RegenerateSurvey=true even if fresh")
	}
	if m.importSurvey != regenerated {
		t.Errorf("Import should receive regenerated survey")
	}
}

// --- Early-exit scenarios ----------------------------------------------------

func TestRunOneShot_SurveyOnlyExitsBeforeImport(t *testing.T) {
	m := &mockImporter{}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
		SurveyOnly: true,
	})
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if m.surveyCalls != 1 {
		t.Errorf("Survey should run in survey-only mode")
	}
	if m.importCalls != 0 {
		t.Errorf("Import must not run in survey-only mode (calls=%d)", m.importCalls)
	}
	if m.loadManifest != 0 {
		t.Errorf("Manifest load must not happen in survey-only mode")
	}
}

func TestRunOneShot_CompleteManifestExitsImmediately(t *testing.T) {
	m := &mockImporter{
		existingSurvey:   &fakeSurvey{stale: false},
		existingManifest: &fakeManifest{status: "complete"},
	}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if m.importCalls != 0 {
		t.Errorf("Import must not run for complete manifest")
	}
}

// --- Resume scenarios --------------------------------------------------------

func TestRunOneShot_ResumeFromInterrupted(t *testing.T) {
	m := &mockImporter{
		existingSurvey:   &fakeSurvey{stale: false},
		existingManifest: &fakeManifest{status: "interrupted", offset: 12345, ids: []string{"<a>", "<b>"}},
		checkpointExists: true,
	}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if m.importCalls != 1 {
		t.Fatalf("Import not called")
	}
	if m.importDecided.Action != Resume {
		t.Errorf("decision.Action = %v, want Resume", m.importDecided.Action)
	}
	if m.importDecided.ByteOffset != 12345 {
		t.Errorf("decision.ByteOffset = %d, want 12345", m.importDecided.ByteOffset)
	}
	if len(m.importDecided.PreservedMessageIDs) != 2 {
		t.Errorf("decision.PreservedMessageIDs len = %d, want 2", len(m.importDecided.PreservedMessageIDs))
	}
}

func TestRunOneShot_FailedRequiresExplicitResume(t *testing.T) {
	m := &mockImporter{
		existingSurvey:   &fakeSurvey{stale: false},
		existingManifest: &fakeManifest{status: "failed"},
		checkpointExists: true,
	}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if err == nil {
		t.Fatalf("RunOneShot should error for failed manifest without --resume")
	}
	if m.importCalls != 0 {
		t.Errorf("Import must not run when explicit resume is required")
	}
}

func TestRunOneShot_FailedWithExplicitResumeContinues(t *testing.T) {
	yes := true
	m := &mockImporter{
		existingSurvey:   &fakeSurvey{stale: false},
		existingManifest: &fakeManifest{status: "failed", offset: 999, ids: []string{"<x>"}},
		checkpointExists: true,
	}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath:     "/tmp/archive.mbox",
		ResumeOverride: &yes,
	})
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if m.importCalls != 1 {
		t.Fatalf("Import should run when --resume=true overrides failed state")
	}
	if m.importDecided.Action != Resume {
		t.Errorf("decision.Action = %v, want Resume", m.importDecided.Action)
	}
	if m.importDecided.ByteOffset != 999 {
		t.Errorf("decision.ByteOffset = %d, want 999", m.importDecided.ByteOffset)
	}
}

func TestRunOneShot_OverrideFalseClearsStateAndStartsFresh(t *testing.T) {
	no := false
	m := &mockImporter{
		existingSurvey:   &fakeSurvey{stale: false},
		existingManifest: &fakeManifest{status: "interrupted", offset: 5555},
		checkpointExists: true,
	}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath:     "/tmp/archive.mbox",
		ResumeOverride: &no,
	})
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if m.clearState != 1 {
		t.Errorf("ClearState should be invoked for --resume=false (calls=%d)", m.clearState)
	}
	if m.importDecided.Action != StartFresh {
		t.Errorf("decision.Action = %v, want StartFresh", m.importDecided.Action)
	}
	if m.importDecided.ByteOffset != 0 {
		t.Errorf("fresh start must not carry byte offset (got %d)", m.importDecided.ByteOffset)
	}
}

// --- Error propagation -------------------------------------------------------

func TestRunOneShot_SurveyErrorPropagates(t *testing.T) {
	boom := errors.New("survey boom")
	m := &mockImporter{surveyErr: boom}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected survey error to propagate, got %v", err)
	}
	if m.importCalls != 0 {
		t.Errorf("Import must not run after survey failure")
	}
}

func TestRunOneShot_ImportErrorPropagates(t *testing.T) {
	boom := errors.New("import boom")
	m := &mockImporter{importErr: boom}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected import error to propagate, got %v", err)
	}
}

func TestRunOneShot_ManifestLoadErrorPropagates(t *testing.T) {
	boom := errors.New("manifest boom")
	m := &mockImporter{manifestErr: boom}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected manifest error to propagate, got %v", err)
	}
}

func TestRunOneShot_FilterLoadErrorPropagates(t *testing.T) {
	boom := errors.New("filter boom")
	m := &mockImporter{filterErr: boom}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
		FilterPath: "/tmp/filter.json",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected filter error to propagate, got %v", err)
	}
	if m.importCalls != 0 {
		t.Errorf("Import must not run after filter-load failure")
	}
}

func TestRunOneShot_SourcePathRequired(t *testing.T) {
	m := &mockImporter{}
	err := RunOneShot(context.Background(), testFramework(), m, RunConfig{})
	if err == nil {
		t.Fatalf("RunOneShot must error on empty SourcePath")
	}
}

func TestRunOneShot_NilImporterErrors(t *testing.T) {
	err := RunOneShot(context.Background(), testFramework(), nil, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if err == nil {
		t.Fatalf("RunOneShot must error on nil Importer")
	}
}

func TestRunOneShot_ContextCancelledBeforeImport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := &mockImporter{}
	err := RunOneShot(ctx, testFramework(), m, RunConfig{
		SourcePath: "/tmp/archive.mbox",
	})
	if err == nil {
		t.Fatalf("expected context-cancelled error, got nil")
	}
}
