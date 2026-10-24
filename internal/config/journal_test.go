package config

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const journalJSON = `{
  "journal": {
    "enabled": true,
    "database": {"host": "postgres-rw.databases-app.svc", "user_file": "%s/username", "password_file": "%s/password"},
    "roster": {"path": "/etc/glovebox-roster/household.json"},
    "services": {
      "openclaw/openclaw-gateway": {"read": true, "readers": "*", "counts": true},
      "openclaw/glovebox-glovebox-connector-powerschool": {"write_types": ["assignment"], "write_scopes": {"source": ["powerschool"]}, "write_subjects": "roster_subjects"}
    },
    "record_types": [
      {"name": "assignment", "schema_version": 1, "owner": "powerschool", "fields": {"title": {"type": "string"}}, "default_audience": ["subject", "guardians"]}
    ],
    "rules": [],
    "staleness_prefixes": ["source=powerschool/"]
  }
}`

func writeJournalConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "username"), []byte("glovebox_journal\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "password"), []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(body, "%s", dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// withJournal inserts extra members at the start of the journal object.
func withJournal(extra string) string {
	return strings.Replace(journalJSON, `"enabled": true,`, `"enabled": true, `+extra+`,`, 1)
}

// loadValidate loads and validates; err is from whichever step refused.
func loadValidate(t *testing.T, body string) error {
	t.Helper()
	cfg, err := LoadConfig(writeJournalConfig(t, body))
	if err != nil {
		return err
	}
	return cfg.Validate()
}

func expect(t *testing.T, name string, ok bool, err error) {
	t.Helper()
	if ok && err != nil {
		t.Errorf("%s: refused: %v", name, err)
	}
	if !ok && err == nil {
		t.Errorf("%s: accepted", name)
	}
}

func TestJournalDefaultsAndDisabled(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	j := cfg.Journal
	if j.Enabled || j.Port != 9095 || j.Database.Name != "glovebox_journal" || j.Database.SSLMode != "require" ||
		j.Roster.Format != "openclaw-household-v1" || j.RemovalGuard.MaxFraction != 0.3 || j.RemovalGuard.MinKeys != 3 ||
		j.ScannerVersion == "" || j.ScanTimeoutSeconds != 10 || j.RequestTimeoutSeconds != 60 || j.Roster.ReloadSeconds != 10 ||
		j.Database.Port != 5432 {
		t.Errorf("defaults %+v", j)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a disabled journal is not validated: %v", err)
	}
}

func TestJournalEnabledValidatesAndBuilds(t *testing.T) {
	cfg, err := LoadConfig(writeJournalConfig(t, journalJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	reg, rs, pol, err := cfg.Journal.Build()
	if err != nil || rs == nil || len(pol.Callers()) != 2 {
		t.Fatalf("%v %v %v", reg, pol, err)
	}
	if _, ok := reg.Active("assignment"); !ok {
		t.Error("record types not built")
	}
	svc, ok := pol.Service("openclaw/openclaw-gateway")
	if !ok || !svc.Read || !svc.Readers.Any || !svc.Counts {
		t.Errorf("policy not carried through: %+v", svc)
	}
	dsn, password, err := cfg.Journal.Database.Connection()
	if err != nil {
		t.Fatal(err)
	}
	if dsn != "postgres://glovebox_journal@postgres-rw.databases-app.svc:5432/glovebox_journal?sslmode=require" {
		t.Errorf("dsn %s", dsn)
	}
	if strings.Contains(dsn, "s3cret") {
		t.Error("the password is in the DSN")
	}
	if p, err := password(context.Background()); err != nil || p != "s3cret" {
		t.Errorf("password %q %v", p, err)
	}
}

func TestJournalAccessors(t *testing.T) {
	cfg, err := LoadConfig(writeJournalConfig(t, withJournal(`"scan_timeout_seconds": 7, "request_timeout_seconds": 11, "scanner_version": "v9"`)))
	if err != nil {
		t.Fatal(err)
	}
	j := cfg.Journal
	if j.ScanTimeout() != 7*time.Second || j.RequestTimeout() != 11*time.Second || j.Roster.Reload() != 10*time.Second {
		t.Errorf("durations %v %v %v", j.ScanTimeout(), j.RequestTimeout(), j.Roster.Reload())
	}
	if g := j.Guard(); g.MaxFraction != 0.3 || g.MinKeys != 3 {
		t.Errorf("guard %+v", g)
	}
	if j.ScannerVersion != "v9" {
		t.Errorf("scanner version %q", j.ScannerVersion)
	}
}

func TestJournalRefusals(t *testing.T) {
	for name, mod := range map[string]func(string) string{
		"no host": func(s string) string {
			return strings.Replace(s, `"host": "postgres-rw.databases-app.svc"`, `"host": ""`, 1)
		},
		"no roster": func(s string) string {
			return strings.Replace(s, `"path": "/etc/glovebox-roster/household.json"`, `"path": ""`, 1)
		},
		"bad roster format": func(s string) string { return strings.Replace(s, `"roster": {`, `"roster": {"format": "csv", `, 1) },
		"port collision": func(s string) string {
			return strings.Replace(s, `"enabled": true,`, `"enabled": true, "port": 9090,`, 1)
		},
		"policy unknown type": func(s string) string {
			return strings.Replace(s, `["assignment"], "write_scopes"`, `["grades"], "write_scopes"`, 1)
		},
		"bad record type":  func(s string) string { return strings.Replace(s, `"type": "string"`, `"type": "blob"`, 1) },
		"bad prefix":       func(s string) string { return strings.Replace(s, `"source=powerschool/"`, `"powerschool"`, 1) },
		"bad sslmode":      func(s string) string { return strings.Replace(s, `"host":`, `"sslmode": "maybe", "host":`, 1) },
		"no user file":     func(s string) string { return strings.Replace(s, `"user_file": "%s/username", `, ``, 1) },
		"no password file": func(s string) string { return strings.Replace(s, `, "password_file": "%s/password"`, ``, 1) },
		"rule on unknown type": func(s string) string {
			return strings.Replace(s, `"rules": []`, `"rules": [{"name":"r","match":{"record_type":"nope"},"importance":"high"}]`, 1)
		},
		"group_by on a text field": func(s string) string {
			return strings.Replace(s, `"rules": []`, `"rules": [{"name":"r","match":{"record_type":"assignment"},"importance":"high","group_by":["title"]}]`, 1)
		},
	} {
		expect(t, name, false, loadValidate(t, mod(journalJSON)))
	}
}

// Each privacy / fail-closed setting is tested on both sides: the value just
// inside the limit is accepted, the value just outside is refused.
func TestJournalFailClosedSettings(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		ok          bool
	}{
		{"scanner version set", `"scanner_version": "x"`, true},
		{"scanner version empty", `"scanner_version": ""`, false},
		{"scanner version blank", `"scanner_version": "  "`, false},
		{"scan timeout 1", `"scan_timeout_seconds": 1`, true},
		{"scan timeout 0", `"scan_timeout_seconds": 0`, false},
		{"scan timeout negative", `"scan_timeout_seconds": -5`, false},
		{"request timeout 1", `"request_timeout_seconds": 1`, true},
		{"request timeout 0", `"request_timeout_seconds": 0`, false},
		{"request timeout negative", `"request_timeout_seconds": -1`, false},
		{"guard fraction just above 0", `"removal_guard": {"max_fraction": 0.0001, "min_keys": 3}`, true},
		{"guard fraction just below 1", `"removal_guard": {"max_fraction": 0.9999, "min_keys": 3}`, true},
		{"guard fraction 0", `"removal_guard": {"max_fraction": 0, "min_keys": 3}`, false},
		{"guard fraction 1", `"removal_guard": {"max_fraction": 1, "min_keys": 3}`, false},
		{"guard fraction above 1", `"removal_guard": {"max_fraction": 1.5, "min_keys": 3}`, false},
		{"guard fraction negative", `"removal_guard": {"max_fraction": -0.1, "min_keys": 3}`, false},
		{"guard min_keys 0", `"removal_guard": {"max_fraction": 0.3, "min_keys": 0}`, true},
		{"guard min_keys negative", `"removal_guard": {"max_fraction": 0.3, "min_keys": -1}`, false},
		{"port 1", `"port": 1`, true},
		{"port 65535", `"port": 65535`, true},
		{"port 0", `"port": 0`, false},
		{"port 65536", `"port": 65536`, false},
		{"port negative", `"port": -1`, false},
	} {
		expect(t, tc.name, tc.ok, loadValidate(t, withJournal(tc.extra)))
	}
}

func TestJournalNestedSettings(t *testing.T) {
	for name, tc := range map[string]struct {
		old, new string
		ok       bool
	}{
		"reload 1":       {`"roster": {`, `"roster": {"reload_seconds": 1, `, true},
		"reload 0":       {`"roster": {`, `"roster": {"reload_seconds": 0, `, false},
		"reload -1":      {`"roster": {`, `"roster": {"reload_seconds": -1, `, false},
		"db port 0":      {`"host":`, `"port": 0, "host":`, false},
		"db port 5433":   {`"host":`, `"port": 5433, "host":`, true},
		"db port 65535":  {`"host":`, `"port": 65535, "host":`, true},
		"db port 65536":  {`"host":`, `"port": 65536, "host":`, false},
		"sslmode verify": {`"host":`, `"sslmode": "verify-full", "host":`, true},
		"sslmode ca":     {`"host":`, `"sslmode": "verify-ca", "host":`, true},
		"sslmode off":    {`"host":`, `"sslmode": "disable", "host":`, true},
		"sslmode empty":  {`"host":`, `"sslmode": "", "host":`, false},
		"no db name":     {`"host":`, `"name": "", "host":`, false},
	} {
		expect(t, name, tc.ok, loadValidate(t, strings.Replace(journalJSON, tc.old, tc.new, 1)))
	}
}

func TestJournalPortCollisions(t *testing.T) {
	for name, tc := range map[string]struct {
		top  string // top-level members placed before "journal"
		port int
		ok   bool
	}{
		"metrics":        {``, 9090, false},
		"ingest":         {``, 9091, false},
		"ingest tls":     {`"ingest": {"tls": {"mode": "required", "cert_file": "a", "key_file": "b", "client_ca_file": "c"}},`, 9092, false},
		"free":           {``, 9100, true},
		"bearer":         {`"ingest": {"bearer_port": 9200},`, 9200, false},
		"ingest off":     {`"ingest": {"enabled": false},`, 9091, true},
		"custom metrics": {`"metrics_port": 9300,`, 9300, false},
	} {
		body := strings.Replace(withJournal(`"port": `+strconv.Itoa(tc.port)), `{`, `{`+tc.top, 1)
		cfg, err := LoadConfig(writeJournalConfig(t, body))
		if err != nil {
			t.Errorf("%s: load: %v", name, err)
			continue
		}
		expect(t, name, tc.ok, cfg.Validate())
	}
}

func TestJournalStrictDecoding(t *testing.T) {
	dupJournal := `{"journal": {"enabled": false}, "journal": ` + journalJSON[strings.Index(journalJSON, `"journal":`)+len(`"journal":`):]
	for name, body := range map[string]string{
		"unknown field":          withJournal(`"extra": 1`),
		"misspelled field":       withJournal(`"scan_timeout_second": 5`),
		"case variant key":       withJournal(`"Port": 9100`),
		"duplicate key":          withJournal(`"port": 9100, "port": 9101`),
		"case variant dup key":   withJournal(`"port": 9100, "PORT": 9101`),
		"nested unknown":         strings.Replace(journalJSON, `"roster": {`, `"roster": {"bogus": 1, `, 1),
		"policy misspelled":      strings.Replace(journalJSON, `"counts": true`, `"count": true`, 1),
		"policy case variant":    strings.Replace(journalJSON, `"counts": true`, `"Counts": true`, 1),
		"rule unknown":           strings.Replace(journalJSON, `"rules": []`, `"rules": [{"name":"r","match":{"record_type":"assignment"},"importance":"high","bogus":1}]`, 1),
		"record type unknown":    strings.Replace(journalJSON, `"owner":`, `"ownr": "x", "owner":`, 1),
		"duplicate caller":       strings.Replace(journalJSON, `"services": {`, `"services": {"openclaw/openclaw-gateway": {}, `, 1),
		"case variant caller":    strings.Replace(journalJSON, `"services": {`, `"services": {"OpenClaw/openclaw-gateway": {}, `, 1),
		"duplicate journal":      dupJournal,
		"case variant journal":   strings.Replace(journalJSON, `"journal":`, `"Journal":`, 1),
		"password in config":     strings.Replace(journalJSON, `"host":`, `"password": "hunter2hunter2", "host":`, 1),
		"database url in config": strings.Replace(journalJSON, `"host":`, `"url": "postgres://u:hunter2hunter2@h/db", "host":`, 1),
	} {
		err := loadValidate(t, body)
		expect(t, name, false, err)
		if err != nil && strings.Contains(err.Error(), "hunter2hunter2") {
			t.Errorf("%s: the error echoes a secret: %v", name, err)
		}
	}
}

func TestJournalBlockOnlyStrictWhenPresent(t *testing.T) {
	// Settings outside journal keep their historic leniency.
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte(`{"some_future_key": 1, "Journal_other": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Errorf("non-journal config became strict: %v", err)
	}
}

func TestJournalSecrets(t *testing.T) {
	load := func(t *testing.T) Config {
		t.Helper()
		cfg, err := LoadConfig(writeJournalConfig(t, journalJSON))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	t.Run("password file is read on every call", func(t *testing.T) {
		cfg := load(t)
		_, password, err := cfg.Journal.Database.Connection()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfg.Journal.Database.PasswordFile, []byte("rotated\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if p, err := password(context.Background()); err != nil || p != "rotated" {
			t.Errorf("password %q %v", p, err)
		}
	})
	t.Run("only the line ending is trimmed", func(t *testing.T) {
		cfg := load(t)
		_ = os.WriteFile(cfg.Journal.Database.PasswordFile, []byte(" pa ss \r\n"), 0o600)
		_, password, _ := cfg.Journal.Database.Connection()
		if p, _ := password(context.Background()); p != " pa ss " {
			t.Errorf("password %q", p)
		}
	})
	t.Run("empty password file refused", func(t *testing.T) {
		cfg := load(t)
		_ = os.WriteFile(cfg.Journal.Database.PasswordFile, []byte("\n"), 0o600)
		if err := cfg.Validate(); err == nil {
			t.Error("an empty password passed Validate")
		}
		_, password, err := cfg.Journal.Database.Connection()
		if err != nil {
			t.Fatal(err)
		}
		if p, err := password(context.Background()); err == nil {
			t.Errorf("an empty password was returned: %q", p)
		}
	})
	t.Run("missing password file refused", func(t *testing.T) {
		cfg := load(t)
		_ = os.Remove(cfg.Journal.Database.PasswordFile)
		if err := cfg.Validate(); err == nil {
			t.Error("missing password file accepted")
		}
	})
	t.Run("missing user file refused", func(t *testing.T) {
		cfg := load(t)
		_ = os.Remove(cfg.Journal.Database.UserFile)
		if err := cfg.Validate(); err == nil {
			t.Error("missing user file accepted")
		}
		if _, _, err := cfg.Journal.Database.Connection(); err == nil {
			t.Error("Connection accepted a missing user file")
		}
	})
	t.Run("empty user file refused", func(t *testing.T) {
		cfg := load(t)
		_ = os.WriteFile(cfg.Journal.Database.UserFile, []byte("\n"), 0o600)
		if err := cfg.Validate(); err == nil {
			t.Error("an empty user passed Validate")
		}
		if _, _, err := cfg.Journal.Database.Connection(); err == nil {
			t.Error("Connection accepted an empty user")
		}
	})
	t.Run("errors never carry the password", func(t *testing.T) {
		cfg := load(t)
		cfg.Journal.Database.SSLMode = "maybe"
		err := cfg.Validate()
		if err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%v", err)
		}
	})
	t.Run("special characters in the user are escaped", func(t *testing.T) {
		cfg := load(t)
		_ = os.WriteFile(cfg.Journal.Database.UserFile, []byte("a b@c:d/e\n"), 0o600)
		dsn, _, err := cfg.Journal.Database.Connection()
		if err != nil || !strings.HasPrefix(dsn, "postgres://a%20b%40c%3Ad%2Fe@postgres-rw.databases-app.svc:5432/") {
			t.Errorf("%s %v", dsn, err)
		}
	})
	t.Run("the user is trimmed of surrounding blanks", func(t *testing.T) {
		cfg := load(t)
		_ = os.WriteFile(cfg.Journal.Database.UserFile, []byte("  app \t\n"), 0o600)
		dsn, _, err := cfg.Journal.Database.Connection()
		if err != nil || !strings.HasPrefix(dsn, "postgres://app@") {
			t.Errorf("%s %v", dsn, err)
		}
	})
	t.Run("sslrootcert reaches the DSN", func(t *testing.T) {
		cfg, err := LoadConfig(writeJournalConfig(t, strings.Replace(journalJSON, `"host":`, `"sslrootcert": "/etc/ca.pem", "host":`, 1)))
		if err != nil {
			t.Fatal(err)
		}
		dsn, _, err := cfg.Journal.Database.Connection()
		if err != nil || !strings.Contains(dsn, "sslrootcert=%2Fetc%2Fca.pem") {
			t.Errorf("%s %v", dsn, err)
		}
	})
}
