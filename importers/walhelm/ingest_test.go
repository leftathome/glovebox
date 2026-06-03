package main

import (
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/ingest"
	"github.com/leftathome/glovebox/internal/ingest/archives"
)

// makeReceipt builds a FinalizeReceipt for tests. acq may be nil to test
// the nil-Acquisition guard.
func makeReceipt(archiveID, dataSubject string, audience []string, acq *ingest.Identity) *archives.FinalizeReceipt {
	return &archives.FinalizeReceipt{
		ArchiveID:   archiveID,
		DataSubject: dataSubject,
		Audience:    audience,
		Acquisition: acq,
	}
}

func newWalhelmMatcher(rules []connector.Rule) *connector.RuleMatcher {
	return connector.NewRuleMatcher(rules)
}

// --- classifyContentType tests -----------------------------------------------

func TestClassifyContentType_EML(t *testing.T) {
	got := classifyContentType("foo/bar/message.eml")
	if got != "message/rfc822" {
		t.Errorf("classifyContentType(.eml) = %q, want %q", got, "message/rfc822")
	}
}

func TestClassifyContentType_JSON(t *testing.T) {
	got := classifyContentType("lab/result.json")
	if got != "application/json" {
		t.Errorf("classifyContentType(.json) = %q, want %q", got, "application/json")
	}
}

func TestClassifyContentType_ZIP(t *testing.T) {
	got := classifyContentType("attachments/archive.zip")
	if got != "application/zip" {
		t.Errorf("classifyContentType(.zip) = %q, want %q", got, "application/zip")
	}
}

func TestClassifyContentType_Unknown(t *testing.T) {
	got := classifyContentType("data/blob.bin")
	if got != "application/octet-stream" {
		t.Errorf("classifyContentType(.bin) = %q, want %q", got, "application/octet-stream")
	}
}

func TestClassifyContentType_NoExtension(t *testing.T) {
	got := classifyContentType("data/noext")
	if got != "application/octet-stream" {
		t.Errorf("classifyContentType(noext) = %q, want %q", got, "application/octet-stream")
	}
}

// --- ruleKeyForEntry tests ----------------------------------------------------

func TestRuleKeyForEntry_MessageDir(t *testing.T) {
	e := walhelmEntry{RelPath: "message/inbox/0001.eml", ContentType: "message/rfc822"}
	got := ruleKeyForEntry(e)
	want := "walhelm:message"
	if got != want {
		t.Errorf("ruleKeyForEntry = %q, want %q", got, want)
	}
}

func TestRuleKeyForEntry_LabDir(t *testing.T) {
	e := walhelmEntry{RelPath: "lab/2026/result.json", ContentType: "application/json"}
	got := ruleKeyForEntry(e)
	want := "walhelm:lab"
	if got != want {
		t.Errorf("ruleKeyForEntry = %q, want %q", got, want)
	}
}

func TestRuleKeyForEntry_TopLevelFile(t *testing.T) {
	// A file directly in tree/ root: no top-level subdir, extension used.
	e := walhelmEntry{RelPath: "record.json", ContentType: "application/json"}
	got := ruleKeyForEntry(e)
	want := "walhelm:record"
	if got != want {
		t.Errorf("ruleKeyForEntry(record.json) = %q, want %q", got, want)
	}
}

// --- BuildItemOptions: full happy-path test -----------------------------------

func TestBuildItemOptions_HappyPath(t *testing.T) {
	acq := &ingest.Identity{
		Provider:   "kp-wa",
		AuthMethod: "browser_session",
		AccountID:  "leftathome",
	}
	receipt := makeReceipt("arch1", "walhelm:9f2a", []string{"subject"}, acq)

	matcher := newWalhelmMatcher([]connector.Rule{
		{Match: "*", Destination: "health-agent"},
	})

	entry := walhelmEntry{RelPath: "lab/2026/bloodwork.json", ContentType: "application/json"}

	opts, err := BuildItemOptions(entry, receipt, matcher, "walhelm-src")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if opts.DataSubject != "walhelm:9f2a" {
		t.Errorf("DataSubject = %q, want %q", opts.DataSubject, "walhelm:9f2a")
	}
	if len(opts.Audience) != 1 || opts.Audience[0] != "subject" {
		t.Errorf("Audience = %v, want [\"subject\"]", opts.Audience)
	}
	if opts.Identity == nil {
		t.Fatal("Identity is nil, want non-nil")
	}
	if opts.Identity.Provider != "kp-wa" {
		t.Errorf("Identity.Provider = %q, want %q", opts.Identity.Provider, "kp-wa")
	}
	if opts.Identity.AuthMethod != "browser_session" {
		t.Errorf("Identity.AuthMethod = %q, want %q", opts.Identity.AuthMethod, "browser_session")
	}
	if opts.Identity.AccountID != "leftathome" {
		t.Errorf("Identity.AccountID = %q, want %q", opts.Identity.AccountID, "leftathome")
	}
	if opts.DestinationAgent != "health-agent" {
		t.Errorf("DestinationAgent = %q, want %q", opts.DestinationAgent, "health-agent")
	}
	want := "arch1:lab/2026/bloodwork.json"
	if opts.Tags["origin_archive"] != want {
		t.Errorf("Tags[origin_archive] = %q, want %q", opts.Tags["origin_archive"], want)
	}
	if opts.Source != "walhelm-src" {
		t.Errorf("Source = %q, want %q", opts.Source, "walhelm-src")
	}
	if opts.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want %q", opts.ContentType, "application/json")
	}
}

// --- BuildItemOptions: ContentType carried from entry -------------------------

func TestBuildItemOptions_ContentTypeFromEntry(t *testing.T) {
	receipt := makeReceipt("arch2", "subj", []string{"agent"}, &ingest.Identity{Provider: "p", AuthMethod: "m", AccountID: "a"})
	matcher := newWalhelmMatcher([]connector.Rule{{Match: "*", Destination: "dst"}})

	entry := walhelmEntry{RelPath: "message/mail.eml", ContentType: "message/rfc822"}
	opts, err := BuildItemOptions(entry, receipt, matcher, "src")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.ContentType != "message/rfc822" {
		t.Errorf("ContentType = %q, want %q", opts.ContentType, "message/rfc822")
	}
}

// --- BuildItemOptions: receipt authority beats rule fields --------------------
//
// This test proves that when the matched rule carries its own data_subject
// and audience, the returned ItemOptions still uses the RECEIPT values --
// not the matcher's. The receipt producer is the authority on subject/audience.
func TestBuildItemOptions_ReceiptIsSubjectAuthority(t *testing.T) {
	receipt := makeReceipt("archX", "receipt-subject", []string{"receipt-audience"}, nil)

	// Rule carries conflicting data_subject / audience.
	matcher := newWalhelmMatcher([]connector.Rule{
		{
			Match:       "*",
			Destination: "agent",
			DataSubject: "rule-subject",
			Audience:    []string{"rule-audience"},
		},
	})

	entry := walhelmEntry{RelPath: "lab/x.json", ContentType: "application/json"}
	opts, err := BuildItemOptions(entry, receipt, matcher, "src")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Must come from receipt, not rule.
	if opts.DataSubject != "receipt-subject" {
		t.Errorf("DataSubject = %q, want %q (must come from receipt)", opts.DataSubject, "receipt-subject")
	}
	if len(opts.Audience) != 1 || opts.Audience[0] != "receipt-audience" {
		t.Errorf("Audience = %v, want [\"receipt-audience\"] (must come from receipt)", opts.Audience)
	}
	// DestinationAgent must still come from the matcher.
	if opts.DestinationAgent != "agent" {
		t.Errorf("DestinationAgent = %q, want %q", opts.DestinationAgent, "agent")
	}
}

// --- BuildItemOptions: nil Acquisition does not panic -------------------------

func TestBuildItemOptions_NilAcquisition(t *testing.T) {
	receipt := makeReceipt("archY", "subj", nil, nil) // Acquisition == nil
	matcher := newWalhelmMatcher([]connector.Rule{{Match: "*", Destination: "dst"}})
	entry := walhelmEntry{RelPath: "lab/data.json", ContentType: "application/json"}

	opts, err := BuildItemOptions(entry, receipt, matcher, "src")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Identity != nil {
		t.Errorf("Identity = %+v, want nil when Acquisition is nil", opts.Identity)
	}
}

// --- BuildItemOptions: unmatched rule + no wildcard -> error ------------------

func TestBuildItemOptions_UnmatchedNoWildcard(t *testing.T) {
	receipt := makeReceipt("archZ", "subj", nil, nil)
	matcher := newWalhelmMatcher([]connector.Rule{
		{Match: "walhelm:message", Destination: "messaging"},
	})

	// lab entry won't match the message rule and there's no wildcard.
	entry := walhelmEntry{RelPath: "lab/result.json", ContentType: "application/json"}

	_, err := BuildItemOptions(entry, receipt, matcher, "src")
	if err == nil {
		t.Fatal("expected error when no rule matches and no wildcard, got nil")
	}
	if !strings.Contains(err.Error(), "destination_agent") {
		t.Errorf("error %q does not mention destination_agent", err.Error())
	}
}

// --- BuildItemOptions: matcher consulted for destination only -----------------
//
// Even when the rule has a specific match key (not wildcard), BuildItemOptions
// should use the matcher ONLY for the destination, never for identity fields.
func TestBuildItemOptions_MatcherOnlySetsDest(t *testing.T) {
	acq := &ingest.Identity{Provider: "prov", AuthMethod: "meth", AccountID: "acct"}
	receipt := makeReceipt("archA", "receipt-ds", []string{"aud-a"}, acq)

	matcher := newWalhelmMatcher([]connector.Rule{
		{Match: "walhelm:lab", Destination: "lab-agent"},
	})

	entry := walhelmEntry{RelPath: "lab/x.json", ContentType: "application/json"}
	opts, err := BuildItemOptions(entry, receipt, matcher, "src")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.DestinationAgent != "lab-agent" {
		t.Errorf("DestinationAgent = %q, want %q", opts.DestinationAgent, "lab-agent")
	}
	if opts.DataSubject != "receipt-ds" {
		t.Errorf("DataSubject = %q, want receipt value", opts.DataSubject)
	}
	if opts.Identity == nil || opts.Identity.Provider != "prov" {
		t.Errorf("Identity.Provider = %q, want %q", opts.Identity.Provider, "prov")
	}
}
