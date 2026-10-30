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
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("ca\n"), 0o600); err != nil {
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
		"bad record type": func(s string) string { return strings.Replace(s, `"type": "string"`, `"type": "blob"`, 1) },
		"bad prefix":      func(s string) string { return strings.Replace(s, `"source=powerschool/"`, `"powerschool"`, 1) },
		"prefix names a subject": func(s string) string {
			return strings.Replace(s, `"source=powerschool/"`, `"source=powerschool/surface=assignments/data_subject=e_child1/"`, 1)
		},
		"prefix is a subject": func(s string) string {
			return strings.Replace(s, `"source=powerschool/"`, `"source=powerschool/data_subject=e_child1/"`, 1)
		},
		"prefix names a course": func(s string) string {
			return strings.Replace(s, `"source=powerschool/"`, `"source=powerschool/course=c1/"`, 1)
		},
		"prefix without trailing slash": func(s string) string {
			return strings.Replace(s, `"source=powerschool/"`, `"source=powerschool"`, 1)
		},
		"prefix with empty value": func(s string) string {
			return strings.Replace(s, `"source=powerschool/"`, `"source=/"`, 1)
		},
		"prefix with empty surface": func(s string) string {
			return strings.Replace(s, `"source=powerschool/"`, `"source=powerschool/surface=/"`, 1)
		},
		"prefix three parts": func(s string) string {
			return strings.Replace(s, `"source=powerschool/"`, `"source=powerschool/surface=a/surface=b/"`, 1)
		},
		"prefix not from source": func(s string) string {
			return strings.Replace(s, `"source=powerschool/"`, `"surface=assignments/"`, 1)
		},
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
		{"request timeout 600", `"request_timeout_seconds": 600`, true},
		{"request timeout 601", `"request_timeout_seconds": 601`, false},
		{"scan timeout 300", `"scan_timeout_seconds": 300`, true},
		{"scan timeout 301", `"scan_timeout_seconds": 301`, false},
		{"scan timeout huge", `"scan_timeout_seconds": 9223372036854775807`, false},
		{"min_keys 1000", `"removal_guard": {"max_fraction": 0.3, "min_keys": 1000}`, true},
		{"min_keys 1001", `"removal_guard": {"max_fraction": 0.3, "min_keys": 1001}`, false},
		{"min_keys huge", `"removal_guard": {"max_fraction": 0.3, "min_keys": 9223372036854775807}`, false},
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
		"reload 1":            {`"roster": {`, `"roster": {"reload_seconds": 1, `, true},
		"reload 0":            {`"roster": {`, `"roster": {"reload_seconds": 0, `, false},
		"reload -1":           {`"roster": {`, `"roster": {"reload_seconds": -1, `, false},
		"db port 0":           {`"host":`, `"port": 0, "host":`, false},
		"db port 5433":        {`"host":`, `"port": 5433, "host":`, true},
		"db port 65535":       {`"host":`, `"port": 65535, "host":`, true},
		"db port 65536":       {`"host":`, `"port": 65536, "host":`, false},
		"sslmode verify":      {`"host":`, `"sslmode": "verify-full", "sslrootcert": "%s/ca.pem", "host":`, true},
		"sslmode ca":          {`"host":`, `"sslmode": "verify-ca", "sslrootcert": "%s/ca.pem", "host":`, true},
		"verify no root":      {`"host":`, `"sslmode": "verify-full", "host":`, false},
		"verify-ca no root":   {`"host":`, `"sslmode": "verify-ca", "host":`, false},
		"verify missing root": {`"host":`, `"sslmode": "verify-full", "sslrootcert": "%s/nope.pem", "host":`, false},
		"verify root is dir":  {`"host":`, `"sslmode": "verify-full", "sslrootcert": "%s", "host":`, false},
		"sslmode off":         {`"host":`, `"sslmode": "disable", "host":`, false},
		"reload 300":          {`"roster": {`, `"roster": {"reload_seconds": 300, `, true},
		"reload 301":          {`"roster": {`, `"roster": {"reload_seconds": 301, `, false},
		"reload huge":         {`"roster": {`, `"roster": {"reload_seconds": 9223372036854775807, `, false},
		"prefix bare":         {`["source=powerschool/"]`, `["source="]`, false},
		"prefix sourcex":      {`["source=powerschool/"]`, `["sourcex"]`, false},
		"prefix ok":           {`["source=powerschool/"]`, `["source=a/"]`, true},
		"multi host":          {`"host": "postgres-rw.databases-app.svc"`, `"host": "a.svc,b.svc"`, false},
		"host slash":          {`"host": "postgres-rw.databases-app.svc"`, `"host": "a/b"`, false},
		"host query":          {`"host": "postgres-rw.databases-app.svc"`, `"host": "a?x=1"`, false},
		"sslmode empty":       {`"host":`, `"sslmode": "", "host":`, false},
		"no db name":          {`"host":`, `"name": "", "host":`, false},
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
	t.Run("a whitespace-only secret file is refused", func(t *testing.T) {
		cfg := load(t)
		_ = os.WriteFile(cfg.Journal.Database.PasswordFile, []byte(" \t\n"), 0o600)
		_, password, _ := cfg.Journal.Database.Connection()
		if p, err := password(context.Background()); err == nil {
			t.Errorf("returned %q", p)
		}
		if err := cfg.Validate(); err == nil {
			t.Error("validated")
		}
	})
	t.Run("an oversized secret file is refused", func(t *testing.T) {
		cfg := load(t)
		_ = os.WriteFile(cfg.Journal.Database.PasswordFile, []byte(strings.Repeat("x", 4097)), 0o600)
		_, password, _ := cfg.Journal.Database.Connection()
		if p, err := password(context.Background()); err == nil {
			t.Errorf("returned %d bytes", len(p))
		}
		_ = os.WriteFile(cfg.Journal.Database.PasswordFile, []byte(strings.Repeat("x", 4096)), 0o600)
		if p, err := password(context.Background()); err != nil || len(p) != 4096 {
			t.Errorf("a 4096-byte secret: %d %v", len(p), err)
		}
	})
	t.Run("a device file does not hang", func(t *testing.T) {
		cfg := load(t)
		cfg.Journal.Database.PasswordFile = "/dev/zero"
		_, password, _ := cfg.Journal.Database.Connection()
		if _, err := password(context.Background()); err == nil {
			t.Error("accepted /dev/zero")
		}
	})
	t.Run("a failure never echoes secret content", func(t *testing.T) {
		cfg := load(t)
		_ = os.WriteFile(cfg.Journal.Database.PasswordFile, []byte("%v %s s3cret"), 0o600)
		_ = os.Remove(cfg.Journal.Database.UserFile)
		err := cfg.Validate()
		if err == nil {
			t.Fatal("validated")
		}
		if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "%v %s") {
			t.Errorf("echoes the content: %v", err)
		}
		_, password, _ := cfg.Journal.Database.Connection()
		_ = password
		cfg2 := load(t)
		_ = os.WriteFile(cfg2.Journal.Database.PasswordFile, []byte("%v %s s3cret"), 0o600)
		_ = os.WriteFile(cfg2.Journal.Database.UserFile, []byte(strings.Repeat("u", 5000)), 0o600)
		if err := cfg2.Validate(); err == nil || strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "%v %s") {
			t.Errorf("%v", err)
		}
	})
	t.Run("the configured sslmode and database name reach the DSN", func(t *testing.T) {
		for _, mode := range []string{"require", "verify-ca", "verify-full"} {
			body := strings.Replace(journalJSON, `"host":`, `"sslmode": "`+mode+`", "sslrootcert": "%s/ca.pem", "name": "other_db", "host":`, 1)
			cfg, err := LoadConfig(writeJournalConfig(t, body))
			if err != nil {
				t.Fatal(err)
			}
			dsn, _, err := cfg.Journal.Database.Connection()
			if err != nil || !strings.Contains(dsn, "sslmode="+mode) || !strings.Contains(dsn, "/other_db?") {
				t.Errorf("%s: %s %v", mode, dsn, err)
			}
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

func TestJournalServicesOmitted(t *testing.T) {
	body := strings.Replace(journalJSON, `"services": {`, `"services_x": {`, 1)
	if err := loadValidate(t, body); err == nil {
		t.Error("unknown key accepted")
	}
	// Remove the services member entirely. Build still turns that into an
	// empty policy that grants nothing, but an enabled journal refuses it at
	// Validate: TokenReview would refuse every caller, so the journal would
	// start useless (Task 28 review M-1).
	i := strings.Index(journalJSON, `"services"`)
	j := strings.Index(journalJSON, `"record_types"`)
	cfg, err := LoadConfig(writeJournalConfig(t, journalJSON[:i]+journalJSON[j:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "journal.services must name at least one caller") {
		t.Errorf("an enabled journal with no services validated: %v", err)
	}
	_, _, pol, err := cfg.Journal.Build()
	if err != nil || len(pol.Callers()) != 0 {
		t.Errorf("%v %v", pol, err)
	}
	// An explicitly empty object is refused the same way.
	empty := journalJSON[:i] + `"services": {},` + journalJSON[j:]
	if err := loadValidate(t, empty); err == nil || !strings.Contains(err.Error(), "journal.services must name at least one caller") {
		t.Errorf("services {} validated: %v", err)
	}
}

func TestJournalAccessorsDoNotOverflow(t *testing.T) {
	j := JournalConfig{ScanTimeoutSeconds: 1 << 62, RequestTimeoutSeconds: -1 << 62}
	j.Roster.ReloadSeconds = 1 << 62
	if j.ScanTimeout() <= 0 || j.RequestTimeout() < 0 || j.Roster.Reload() <= 0 {
		t.Errorf("%v %v %v", j.ScanTimeout(), j.RequestTimeout(), j.Roster.Reload())
	}
	if j.ScanTimeout() != time.Hour {
		t.Errorf("clamp %v", j.ScanTimeout())
	}
	j.ScanTimeoutSeconds = 3601
	if j.ScanTimeout() != time.Hour {
		t.Errorf("clamp at 3601: %v", j.ScanTimeout())
	}
	j.ScanTimeoutSeconds, j.Roster.ReloadSeconds = 5, 7
	if j.ScanTimeout() != 5*time.Second || j.Roster.Reload() != 7*time.Second {
		t.Error("small values changed")
	}
}

func TestJournalStalenessPrefixShapes(t *testing.T) {
	for _, p := range []string{"source=powerschool/", "source=powerschool/surface=assignments/"} {
		expect(t, p, true, loadValidate(t, strings.Replace(journalJSON, `"source=powerschool/"`, `"`+p+`"`, 1)))
	}
}

// The chart's rendered journal block loads and validates (regenerate with
// the command in Task 29 of the plan after changing either side):
//
//	helm template glovebox charts/glovebox -n openclaw -f scripts/testdata/journal-values.yaml \
//	  | python3 -c "import sys, yaml; [print(d['data']['config.json']) for d in yaml.safe_load_all(sys.stdin) if d and d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'glovebox-glovebox-config']" \
//	  > internal/config/testdata/helm-rendered-journal.json
func TestJournalHelmRenderedFixture(t *testing.T) {
	cfg, err := LoadConfig("testdata/helm-rendered-journal.json")
	if err != nil {
		t.Fatal(err)
	}
	j := cfg.Journal
	// The chart names the in-container mount paths; pin them before
	// pointing the validator at temporary files.
	if j.Database.UserFile != "/etc/glovebox-journal-db/username" || j.Database.PasswordFile != "/etc/glovebox-journal-db/password" {
		t.Errorf("chart -> Go mismatch on the database file paths: %+v", j.Database)
	}
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "username"), []byte("u"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "password"), []byte("p"), 0o600)
	cfg.Journal.Database.UserFile = filepath.Join(dir, "username")
	cfg.Journal.Database.PasswordFile = filepath.Join(dir, "password")
	// The rendered config names in-container paths (subjects.json, ...);
	// only the journal block is under test here.
	if err := cfg.Journal.validate(cfg.portsInUse()); err != nil {
		t.Fatal(err)
	}
	if !j.Enabled || j.Port != 9095 || j.Roster.Path != "/etc/glovebox-roster/household.json" || len(j.Services) == 0 ||
		len(j.RecordTypes) != 1 || j.RecordTypes[0].Fields["due"].Type != "date" || len(j.StalenessPrefixes) != 1 ||
		j.StalenessPrefixes[0] != "source=powerschool/" || j.ScannerVersion != "core-scan-1" || j.Database.SSLMode != "require" ||
		j.RequestTimeoutSeconds != 60 || j.ScanTimeoutSeconds != 10 || j.RemovalGuard.MaxFraction != 0.3 || j.RemovalGuard.MinKeys != 3 {
		t.Errorf("chart -> Go mismatch: %+v", j)
	}
	_, _, pol, err := cfg.Journal.Build()
	if err != nil || pol == nil {
		t.Fatalf("Build: %v", err)
	}
}

// The core config the chart renders with PowerSchool records on
// (scripts/testdata/records-values.yaml) builds: every write_type of the
// connector's services entry is a record type the core loads. Without
// journal.connectorRecordTypes the core would exit at startup on "unknown
// record type", and the chart refuses that render. Regenerate with:
//
//	helm template glovebox charts/glovebox -n openclaw -f scripts/testdata/records-values.yaml \
//	  | python3 -c "import sys, yaml; [print(d['data']['config.json']) for d in yaml.safe_load_all(sys.stdin) if d and d['kind'] == 'ConfigMap' and d['metadata']['name'] == 'glovebox-glovebox-config']" \
//	  > internal/config/testdata/helm-rendered-records.json
//
// scripts/test-chart-records.sh fails when the fixture and the render drift.
func TestJournalHelmRenderedRecordsFixture(t *testing.T) {
	cfg, err := LoadConfig("testdata/helm-rendered-records.json")
	if err != nil {
		t.Fatal(err)
	}
	reg, _, pol, err := cfg.Journal.Build()
	if err != nil || pol == nil {
		t.Fatalf("Build: %v", err)
	}
	for _, name := range []string{"course_mark", "assignment", "attendance_total", "attendance_mark", "attendance_day", "term_grade", "term_comment"} {
		if _, ok := reg.Active(name); !ok {
			t.Errorf("record type %s not loaded", name)
		}
	}
}
