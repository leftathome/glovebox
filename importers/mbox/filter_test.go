package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mustTime parses an RFC 3339 string for test-table use.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return tm
}

func ptrInt64(v int64) *int64 { return &v }
func ptrTime(v time.Time) *time.Time { return &v }

// --- Per-field match tests -------------------------------------------------

func TestEvaluate_Label(t *testing.T) {
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{Label: "Spam"}, Action: ActionExclude},
		},
	}

	hit := &Message{GmailLabels: []string{"INBOX", "Spam"}}
	miss := &Message{GmailLabels: []string{"INBOX"}}

	if act, idx, key := cfg.Evaluate(hit); act != ActionExclude || idx != 0 || key != "rule_0_label_Spam" {
		t.Fatalf("hit: got (%q, %d, %q)", act, idx, key)
	}
	if act, idx, key := cfg.Evaluate(miss); act != ActionExclude || idx != -1 || key != "default_excluded" {
		t.Fatalf("miss: got (%q, %d, %q)", act, idx, key)
	}
}

func TestEvaluate_ListID_Glob(t *testing.T) {
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{ListID: "*@lwn.net"}, Action: ActionExclude},
		},
	}

	cases := []struct {
		listID string
		want   string
	}{
		{"lwn-announce@lwn.net", ActionExclude},
		{"security@lwn.net", ActionExclude},
		{"dev@example.com", ActionExclude}, // no match -> default_excluded
	}
	for _, c := range cases {
		m := &Message{ListID: c.listID}
		act, _, _ := cfg.Evaluate(m)
		if act != c.want {
			t.Errorf("listID %q: got %q want %q", c.listID, act, c.want)
		}
	}

	// Confirm the rule actually fired for a matching list (not just the
	// default-exclude path).
	if _, idx, key := cfg.Evaluate(&Message{ListID: "announce@lwn.net"}); idx != 0 || key != "rule_0_list_id_*@lwn.net" {
		t.Errorf("idx/key: got (%d, %q)", idx, key)
	}

	// Exact (no glob) also works.
	cfg2 := &FilterConfig{FilterRules: []FilterRule{
		{Match: MatchSpec{ListID: "dev@oldjob.example.com"}, Action: ActionInclude},
	}}
	if act, _, _ := cfg2.Evaluate(&Message{ListID: "dev@oldjob.example.com"}); act != ActionInclude {
		t.Fatalf("exact list_id: got %q", act)
	}
}

func TestEvaluate_Sender_Exact(t *testing.T) {
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{Sender: "boss@oldjob.example.com"}, Action: ActionInclude},
		},
	}
	if act, _, _ := cfg.Evaluate(&Message{From: "boss@oldjob.example.com"}); act != ActionInclude {
		t.Fatal("exact sender should match include")
	}
	// Suffix or prefix should not match.
	if act, _, _ := cfg.Evaluate(&Message{From: "notboss@oldjob.example.com"}); act != ActionExclude {
		t.Fatal("prefix non-match should default-exclude")
	}
	if act, _, _ := cfg.Evaluate(&Message{From: "boss@oldjob.example.com.evil.com"}); act != ActionExclude {
		t.Fatal("suffix non-match should default-exclude")
	}
}

func TestEvaluate_SenderDomain_Glob(t *testing.T) {
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{SenderDomain: "newsletter.*"}, Action: ActionExclude},
		},
	}
	if act, _, _ := cfg.Evaluate(&Message{From: "promo@newsletter.example"}); act != ActionExclude {
		t.Fatal("newsletter.example should match glob newsletter.*")
	}
	if act, _, _ := cfg.Evaluate(&Message{From: "ok@notnewsletter.example"}); act != ActionExclude {
		// default_excluded -> action exclude but idx -1
		if _, idx, _ := cfg.Evaluate(&Message{From: "ok@notnewsletter.example"}); idx != -1 {
			t.Fatal("non-matching domain should take the default-excluded path, not rule 0")
		}
	}
	// Sender with no '@' has empty domain and should not satisfy a domain rule.
	if _, idx, _ := cfg.Evaluate(&Message{From: "malformed-no-at"}); idx != -1 {
		t.Fatal("malformed sender should not satisfy sender_domain rule")
	}
}

func TestEvaluate_SubjectContains_CaseInsensitive(t *testing.T) {
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{SubjectContains: "INVOICE"}, Action: ActionInclude},
		},
	}
	for _, subj := range []string{"invoice attached", "Your INVOICE", "Re: Invoice #42"} {
		if act, _, _ := cfg.Evaluate(&Message{Subject: subj}); act != ActionInclude {
			t.Errorf("subject %q: expected include", subj)
		}
	}
	if act, _, _ := cfg.Evaluate(&Message{Subject: "quarterly report"}); act != ActionExclude {
		t.Fatal("non-matching subject should default-exclude")
	}
}

func TestEvaluate_DateAfter(t *testing.T) {
	cutoff := mustTime(t, "2020-01-01T00:00:00Z")
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{DateAfter: ptrTime(cutoff)}, Action: ActionInclude},
		},
	}

	after := mustTime(t, "2020-06-01T12:00:00Z")
	before := mustTime(t, "2019-06-01T12:00:00Z")
	if act, _, _ := cfg.Evaluate(&Message{Date: after}); act != ActionInclude {
		t.Fatal("date after cutoff should match include")
	}
	if act, _, _ := cfg.Evaluate(&Message{Date: before}); act != ActionExclude {
		t.Fatal("date before cutoff should default-exclude")
	}
	// Zero Date (missing/unparseable) must NOT satisfy date_after (fail-safe).
	if _, idx, _ := cfg.Evaluate(&Message{}); idx != -1 {
		t.Fatal("zero Date must not satisfy date_after rule")
	}
}

func TestEvaluate_DateBefore(t *testing.T) {
	cutoff := mustTime(t, "2020-01-01T00:00:00Z")
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{DateBefore: ptrTime(cutoff)}, Action: ActionExclude},
		},
	}

	before := mustTime(t, "2019-06-01T12:00:00Z")
	after := mustTime(t, "2020-06-01T12:00:00Z")
	if _, idx, _ := cfg.Evaluate(&Message{Date: before}); idx != 0 {
		t.Fatal("before-cutoff should match rule 0")
	}
	if _, idx, _ := cfg.Evaluate(&Message{Date: after}); idx != -1 {
		t.Fatal("after-cutoff should default-exclude")
	}
	if _, idx, _ := cfg.Evaluate(&Message{}); idx != -1 {
		t.Fatal("zero Date must not satisfy date_before rule")
	}
}

func TestEvaluate_MinSizeBytes(t *testing.T) {
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{MinSizeBytes: ptrInt64(1000)}, Action: ActionInclude},
		},
	}
	if _, idx, _ := cfg.Evaluate(&Message{Size: 1000}); idx != 0 {
		t.Fatal("size == min should satisfy min_size_bytes")
	}
	if _, idx, _ := cfg.Evaluate(&Message{Size: 2000}); idx != 0 {
		t.Fatal("size > min should satisfy min_size_bytes")
	}
	if _, idx, _ := cfg.Evaluate(&Message{Size: 500}); idx != -1 {
		t.Fatal("size < min should not satisfy min_size_bytes")
	}
}

func TestEvaluate_MaxSizeBytes(t *testing.T) {
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{MaxSizeBytes: ptrInt64(1000)}, Action: ActionExclude},
		},
	}
	if _, idx, _ := cfg.Evaluate(&Message{Size: 1000}); idx != 0 {
		t.Fatal("size == max should satisfy max_size_bytes")
	}
	if _, idx, _ := cfg.Evaluate(&Message{Size: 500}); idx != 0 {
		t.Fatal("size < max should satisfy max_size_bytes")
	}
	if _, idx, _ := cfg.Evaluate(&Message{Size: 2000}); idx != -1 {
		t.Fatal("size > max should not satisfy max_size_bytes")
	}
}

// --- Semantic tests --------------------------------------------------------

// TestEvaluate_AND_WithinMatchSpec verifies that when a MatchSpec sets
// multiple fields, ALL of them must be satisfied for the rule to fire.
func TestEvaluate_AND_WithinMatchSpec(t *testing.T) {
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{
				Match:  MatchSpec{Label: "INBOX", SenderDomain: "*.example.com"},
				Action: ActionInclude,
			},
		},
	}

	// Both match -> rule fires.
	both := &Message{GmailLabels: []string{"INBOX"}, From: "alice@mail.example.com"}
	if _, idx, _ := cfg.Evaluate(both); idx != 0 {
		t.Fatal("both fields match: should fire rule 0")
	}

	// Label matches, domain does not.
	onlyLabel := &Message{GmailLabels: []string{"INBOX"}, From: "alice@other.com"}
	if _, idx, _ := cfg.Evaluate(onlyLabel); idx != -1 {
		t.Fatal("only label matches: AND fails, should default-exclude")
	}

	// Domain matches, label does not.
	onlyDomain := &Message{GmailLabels: []string{"Spam"}, From: "alice@mail.example.com"}
	if _, idx, _ := cfg.Evaluate(onlyDomain); idx != -1 {
		t.Fatal("only domain matches: AND fails, should default-exclude")
	}
}

// TestEvaluate_FirstMatchWins verifies that earlier rules take precedence
// over later rules with the same applicability.
func TestEvaluate_FirstMatchWins(t *testing.T) {
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{ListID: "dev@oldjob.example.com"}, Action: ActionInclude},
			{Match: MatchSpec{Label: "Spam"}, Action: ActionExclude},
		},
	}

	// A message that satisfies BOTH rules: the first rule wins.
	m := &Message{
		ListID:      "dev@oldjob.example.com",
		GmailLabels: []string{"Spam"},
	}
	act, idx, _ := cfg.Evaluate(m)
	if act != ActionInclude || idx != 0 {
		t.Fatalf("first match wins: got (%q, %d); want (include, 0)", act, idx)
	}

	// Swap order: now the Spam exclude fires first.
	cfg2 := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{Label: "Spam"}, Action: ActionExclude},
			{Match: MatchSpec{ListID: "dev@oldjob.example.com"}, Action: ActionInclude},
		},
	}
	act, idx, _ = cfg2.Evaluate(m)
	if act != ActionExclude || idx != 0 {
		t.Fatalf("swapped order: got (%q, %d); want (exclude, 0)", act, idx)
	}
}

// TestEvaluate_DefaultExclude verifies the no-rule-matches path.
func TestEvaluate_DefaultExclude(t *testing.T) {
	// Empty config.
	empty := &FilterConfig{}
	act, idx, key := empty.Evaluate(&Message{Subject: "anything"})
	if act != ActionExclude || idx != -1 || key != "default_excluded" {
		t.Fatalf("empty config: got (%q, %d, %q)", act, idx, key)
	}

	// Nil receiver.
	var nilCfg *FilterConfig
	act, idx, key = nilCfg.Evaluate(&Message{})
	if act != ActionExclude || idx != -1 || key != "default_excluded" {
		t.Fatalf("nil config: got (%q, %d, %q)", act, idx, key)
	}

	// Non-empty config but nothing matches.
	cfg := &FilterConfig{FilterRules: []FilterRule{
		{Match: MatchSpec{Label: "Spam"}, Action: ActionExclude},
	}}
	act, idx, key = cfg.Evaluate(&Message{GmailLabels: []string{"INBOX"}})
	if act != ActionExclude || idx != -1 || key != "default_excluded" {
		t.Fatalf("no match: got (%q, %d, %q)", act, idx, key)
	}
}

// TestEvaluate_WildcardIncludeTerminator verifies the spec §3.4.3 pattern:
// a trailing `{"action": "include"}` rule acts as an opt-out terminator.
func TestEvaluate_WildcardIncludeTerminator(t *testing.T) {
	cfg := &FilterConfig{
		FilterRules: []FilterRule{
			{Match: MatchSpec{Label: "Spam"}, Action: ActionExclude},
			{Match: MatchSpec{Label: "Trash"}, Action: ActionExclude},
			{Match: MatchSpec{}, Action: ActionInclude}, // wildcard terminator
		},
	}

	// Spam -> excluded by rule 0.
	if act, idx, _ := cfg.Evaluate(&Message{GmailLabels: []string{"Spam"}}); act != ActionExclude || idx != 0 {
		t.Fatalf("Spam: got (%q, %d)", act, idx)
	}
	// Anything else -> included by the wildcard.
	act, idx, key := cfg.Evaluate(&Message{GmailLabels: []string{"INBOX"}})
	if act != ActionInclude || idx != 2 || key != "rule_2_wildcard" {
		t.Fatalf("other: got (%q, %d, %q)", act, idx, key)
	}
	// Message with no labels at all also included.
	act, idx, _ = cfg.Evaluate(&Message{})
	if act != ActionInclude || idx != 2 {
		t.Fatalf("empty message: got (%q, %d)", act, idx)
	}
}

// --- LoadFilter tests ------------------------------------------------------

func TestLoadFilter_NotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	_, err := LoadFilter(path)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error %v does not satisfy errors.Is(err, fs.ErrNotExist)", err)
	}
}

func TestLoadFilter_RejectsUnknownAction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "filter.json")
	body := `{
		"schema_version": "1.0",
		"filter_rules": [
			{"match": {"label": "Spam"}, "action": "burninate"}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFilter(path); err == nil {
		t.Fatal("expected error for unknown action")
	}
}

func TestLoadFilter_RejectsEmptyMatchWithExclude(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "filter.json")
	body := `{
		"schema_version": "1.0",
		"filter_rules": [
			{"match": {}, "action": "exclude"}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFilter(path); err == nil {
		t.Fatal("expected error for empty match + exclude")
	}
}

func TestLoadFilter_AcceptsWildcardInclude(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "filter.json")
	body := `{
		"schema_version": "1.0",
		"filter_rules": [
			{"match": {"label": "Spam"}, "action": "exclude"},
			{"match": {}, "action": "include"}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFilter(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.FilterRules) != 2 {
		t.Fatalf("got %d rules, want 2", len(cfg.FilterRules))
	}
}

func TestLoadFilter_ParsesDatesAndSizes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "filter.json")
	body := `{
		"schema_version": "1.0",
		"filter_rules": [
			{"match": {"date_after": "2020-01-01T00:00:00Z", "max_size_bytes": 20971520}, "action": "include"}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFilter(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := cfg.FilterRules[0]
	if r.Match.DateAfter == nil || !r.Match.DateAfter.Equal(mustTime(t, "2020-01-01T00:00:00Z")) {
		t.Errorf("DateAfter: got %v", r.Match.DateAfter)
	}
	if r.Match.MaxSizeBytes == nil || *r.Match.MaxSizeBytes != 20971520 {
		t.Errorf("MaxSizeBytes: got %v", r.Match.MaxSizeBytes)
	}
}

// --- ruleKey stability -----------------------------------------------------

// TestRuleKeyFormat_Stability verifies that every ruleKey shape we promise
// in the spec is produced deterministically.
func TestRuleKeyFormat_Stability(t *testing.T) {
	cases := []struct {
		name string
		idx  int
		spec MatchSpec
		want string
	}{
		{"label", 0, MatchSpec{Label: "Spam"}, "rule_0_label_Spam"},
		{"list_id", 1, MatchSpec{ListID: "*@lwn.net"}, "rule_1_list_id_*@lwn.net"},
		{"sender", 2, MatchSpec{Sender: "boss@example.com"}, "rule_2_sender_boss@example.com"},
		{"sender_domain", 3, MatchSpec{SenderDomain: "newsletter.*"}, "rule_3_sender_domain_newsletter.*"},
		{"subject_contains", 4, MatchSpec{SubjectContains: "invoice"}, "rule_4_subject_contains_invoice"},
		{
			"date_after", 5,
			MatchSpec{DateAfter: ptrTime(mustTime(t, "2020-01-01T00:00:00Z"))},
			"rule_5_date_after_2020-01-01",
		},
		{
			"date_before", 6,
			MatchSpec{DateBefore: ptrTime(mustTime(t, "2020-01-01T00:00:00Z"))},
			"rule_6_date_before_2020-01-01",
		},
		{"min_size_bytes", 7, MatchSpec{MinSizeBytes: ptrInt64(1024)}, "rule_7_min_size_bytes_1024"},
		{"max_size_bytes", 8, MatchSpec{MaxSizeBytes: ptrInt64(20971520)}, "rule_8_max_size_bytes_20971520"},
		{"wildcard", 9, MatchSpec{}, "rule_9_wildcard"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ruleKeyFor(c.idx, c.spec)
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
			// Determinism: calling twice yields identical results.
			if again := ruleKeyFor(c.idx, c.spec); again != got {
				t.Errorf("ruleKeyFor not deterministic: %q vs %q", got, again)
			}
		})
	}
}

// TestRuleKey_PrecedenceWhenMultipleFieldsSet verifies that the documented
// field precedence is honored, which is what makes keys stable in the face
// of future schema evolution.
func TestRuleKey_PrecedenceWhenMultipleFieldsSet(t *testing.T) {
	// Label takes precedence over all other fields.
	spec := MatchSpec{
		Label:        "INBOX",
		SenderDomain: "*.example.com",
		MinSizeBytes: ptrInt64(100),
	}
	if got := ruleKeyFor(0, spec); got != "rule_0_label_INBOX" {
		t.Errorf("label precedence: got %q", got)
	}
}
