package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/internal/journal/diff"
	"github.com/leftathome/glovebox/internal/journal/policy"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
	"github.com/leftathome/glovebox/internal/journal/strictjson"
)

// defaultScannerVersion labels verdicts the core's scanner gives journal
// text. Bump scanner_version when the scanning rules change so a rescan
// judges stored values again. It is never empty: "" in a stored verdict
// means "judged by more than one scanner, or by none".
const defaultScannerVersion = "core-scan-1"

// JournalConfig configures the record journal (docs/journal-api.md). Off
// unless Enabled. The block is decoded strictly (unknown, duplicate and
// case-variant keys are errors): it holds the access policy, and a key
// that silently vanished would widen a grant. It never holds a secret --
// the database credentials are files from a mounted Secret.
type JournalConfig struct {
	Enabled               bool                  `json:"enabled"`
	Port                  int                   `json:"port"`
	RequestTimeoutSeconds int                   `json:"request_timeout_seconds"`
	Database              JournalDatabaseConfig `json:"database"`
	Roster                JournalRosterConfig   `json:"roster"`
	RemovalGuard          JournalGuardConfig    `json:"removal_guard"`
	ScanTimeoutSeconds    int                   `json:"scan_timeout_seconds"`
	ScannerVersion        string                `json:"scanner_version"`
	// Services is the policy document, kept raw so that Build loads it
	// through policy.Parse (strict) like any other policy document.
	Services    json.RawMessage `json:"services"`
	RecordTypes []record.Type   `json:"record_types"`
	Rules       []rules.Rule    `json:"rules"`
	// StalenessPrefixes are the scope prefixes whose newest snapshot is
	// exported for the "no snapshots" alert (decision 8).
	StalenessPrefixes []string `json:"staleness_prefixes"`
}

// JournalDatabaseConfig locates the journal's PostgreSQL database. The
// user name and password are files from the mounted Secret.
type JournalDatabaseConfig struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Name         string `json:"name"`
	SSLMode      string `json:"sslmode"`
	SSLRootCert  string `json:"sslrootcert"`
	UserFile     string `json:"user_file"`
	PasswordFile string `json:"password_file"`
}

// JournalRosterConfig is the household roster file (a mounted ConfigMap).
type JournalRosterConfig struct {
	Path          string `json:"path"`
	Format        string `json:"format"`
	ReloadSeconds int    `json:"reload_seconds"`
}

// JournalGuardConfig is the removal guard (4.5).
type JournalGuardConfig struct {
	MaxFraction float64 `json:"max_fraction"`
	MinKeys     int     `json:"min_keys"`
}

func defaultJournal() JournalConfig {
	return JournalConfig{
		Port:                  9095,
		RequestTimeoutSeconds: 60,
		Database:              JournalDatabaseConfig{Port: 5432, Name: "glovebox_journal", SSLMode: "require"},
		Roster:                JournalRosterConfig{Format: roster.FormatOpenclawHouseholdV1, ReloadSeconds: 10},
		RemovalGuard:          JournalGuardConfig{MaxFraction: 0.3, MinKeys: 3},
		ScanTimeoutSeconds:    10,
		ScannerVersion:        defaultScannerVersion,
	}
}

// UnmarshalJSON decodes the block strictly over whatever defaults are
// already in *j.
func (j *JournalConfig) UnmarshalJSON(b []byte) error {
	type bare JournalConfig
	return strictjson.Decode(b, (*bare)(j))
}

// checkJournalKey refuses a config whose top level names the journal block
// twice, or with different case. encoding/json would merge the two blocks,
// so a second {"journal": ...} could quietly override the first.
func checkJournalKey(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil // not an object; json.Unmarshal reports it
	}
	seen := 0
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return nil
		}
		key, _ := k.(string)
		if strings.EqualFold(key, "journal") {
			if key != "journal" {
				return fmt.Errorf("config key %q must be spelled exactly \"journal\"", key)
			}
			seen++
			if seen > 1 {
				return fmt.Errorf("config key \"journal\" appears more than once")
			}
		}
		var skip json.RawMessage
		if err := d.Decode(&skip); err != nil && err != io.EOF {
			return nil
		}
	}
	return nil
}

// Build compiles the record types, rules and policy. The policy goes
// through policy.Parse, the only way a policy document is loaded.
func (j JournalConfig) Build() (*record.Registry, *rules.Set, *policy.Policy, error) {
	reg, err := record.NewRegistry(j.RecordTypes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("journal.record_types: %w", err)
	}
	rs, err := rules.Compile(j.Rules, reg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("journal.rules: %w", err)
	}
	raw := j.Services
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	pol, err := policy.Parse(raw, reg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("journal.services: %w", err)
	}
	return reg, rs, pol, nil
}

// Guard is the removal guard as the diff package takes it.
func (j JournalConfig) Guard() diff.Guard {
	return diff.Guard{MaxFraction: j.RemovalGuard.MaxFraction, MinKeys: j.RemovalGuard.MinKeys}
}

// ScanTimeout bounds one text scan.
func (j JournalConfig) ScanTimeout() time.Duration { return seconds(j.ScanTimeoutSeconds) }

// RequestTimeout bounds one API request.
func (j JournalConfig) RequestTimeout() time.Duration { return seconds(j.RequestTimeoutSeconds) }

// Reload is how often the roster file is re-read.
func (r JournalRosterConfig) Reload() time.Duration { return seconds(r.ReloadSeconds) }

// Upper bounds. A huge value would overflow a time.Duration negative: a
// negative reload interval ends the roster watcher (revocations never
// arrive), a huge min_keys switches the removal guard off.
const (
	maxReloadSeconds  = 300
	maxRequestSeconds = 600
	maxScanSeconds    = 300
	maxMinKeys        = 1000
	maxSecretBytes    = 4096
)

// seconds converts without overflow: validated values are small; anything
// else is clamped into [0, 1h] rather than wrapping negative.
func seconds(n int) time.Duration {
	if n < 0 {
		n = 0
	}
	if n > 3600 {
		n = 3600
	}
	return time.Duration(n) * time.Second
}

func (j JournalConfig) validate(portsInUse map[int]string) error {
	if !j.Enabled {
		return nil
	}
	if j.Port <= 0 || j.Port > 65535 {
		return fmt.Errorf("journal.port must be 1-65535, got %d", j.Port)
	}
	if other, ok := portsInUse[j.Port]; ok {
		return fmt.Errorf("journal.port %d is already used by %s", j.Port, other)
	}
	if j.RequestTimeoutSeconds <= 0 || j.RequestTimeoutSeconds > maxRequestSeconds {
		return fmt.Errorf("journal.request_timeout_seconds must be 1-%d", maxRequestSeconds)
	}
	if j.ScanTimeoutSeconds <= 0 || j.ScanTimeoutSeconds > maxScanSeconds {
		return fmt.Errorf("journal.scan_timeout_seconds must be 1-%d", maxScanSeconds)
	}
	if strings.TrimSpace(j.ScannerVersion) == "" {
		return fmt.Errorf("journal.scanner_version must not be empty")
	}
	d := j.Database
	if d.Host == "" || d.Name == "" || d.UserFile == "" || d.PasswordFile == "" || d.Port <= 0 || d.Port > 65535 {
		return fmt.Errorf("journal.database needs host, port (1-65535), name, user_file and password_file")
	}
	switch d.SSLMode {
	case "require":
	case "verify-ca", "verify-full":
		if d.SSLRootCert == "" {
			return fmt.Errorf("journal.database.sslrootcert is required when sslmode is %s", d.SSLMode)
		}
		if st, err := os.Stat(d.SSLRootCert); err != nil || st.IsDir() {
			return fmt.Errorf("journal.database.sslrootcert %s is not a readable file", d.SSLRootCert)
		}
	default:
		return fmt.Errorf("journal.database.sslmode must be require, verify-ca or verify-full; plaintext is refused")
	}
	if strings.ContainsAny(d.Host, ",/?") {
		return fmt.Errorf("journal.database.host must be a single host name (no ',', '/' or '?')")
	}
	if j.Roster.Path == "" || j.Roster.Format != roster.FormatOpenclawHouseholdV1 || j.Roster.ReloadSeconds <= 0 || j.Roster.ReloadSeconds > maxReloadSeconds {
		return fmt.Errorf("journal.roster needs a path, format %q and reload_seconds 1-%d", roster.FormatOpenclawHouseholdV1, maxReloadSeconds)
	}
	// max_fraction >= 1 would let a snapshot remove every live key without
	// being held; <= 0 would hold every removal. Both are refused.
	if !(j.RemovalGuard.MaxFraction > 0 && j.RemovalGuard.MaxFraction < 1) || j.RemovalGuard.MinKeys < 0 || j.RemovalGuard.MinKeys > maxMinKeys {
		return fmt.Errorf("journal.removal_guard needs 0 < max_fraction < 1 and 0 <= min_keys <= %d", maxMinKeys)
	}
	for _, p := range j.StalenessPrefixes {
		if err := checkStalenessPrefix(p); err != nil {
			return err
		}
	}
	// Fail at start, not at the first request, when the mounted Secret is
	// absent or empty. Only the paths are named, never the contents.
	if _, _, err := d.Connection(); err != nil {
		return err
	}
	if _, err := readSecretFile(d.PasswordFile); err != nil {
		return fmt.Errorf("journal database password: %w", err)
	}
	_, _, _, err := j.Build()
	return err
}

// Connection returns the DSN (no password in it) and the callback that
// reads the password from its file, for journal.Open. The file is read on
// every new connection, so a rotated Secret is picked up without a restart.
func (d JournalDatabaseConfig) Connection() (string, func(context.Context) (string, error), error) {
	b, err := readSecretFile(d.UserFile)
	if err != nil {
		return "", nil, fmt.Errorf("journal database user: %w", err)
	}
	user := strings.TrimSpace(b) // readSecretFile already refused an empty file
	q := url.Values{"sslmode": {d.SSLMode}}
	if d.SSLRootCert != "" {
		q.Set("sslrootcert", d.SSLRootCert)
	}
	u := url.URL{Scheme: "postgres", User: url.User(user), Host: net.JoinHostPort(d.Host, strconv.Itoa(d.Port)),
		Path: "/" + d.Name, RawQuery: q.Encode()}
	file := d.PasswordFile
	return u.String(), func(context.Context) (string, error) {
		p, err := readSecretFile(file)
		if err != nil {
			return "", fmt.Errorf("journal database password: %w", err)
		}
		return p, nil
	}, nil
}

// readSecretFile returns the file's content without its trailing line
// ending (mounted Secrets often carry one; a password may contain other
// spaces). An empty result is an error. Errors name the path only.
func readSecretFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s: read failed", path)
	}
	if len(b) > maxSecretBytes {
		return "", fmt.Errorf("%s is larger than %d bytes", path, maxSecretBytes)
	}
	s := strings.TrimRight(string(b), "\r\n")
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

// checkStalenessPrefix refuses a staleness prefix that could carry data.
// A prefix becomes the `prefix` label of a public Prometheus series, so it
// may name a source and at most one surface and nothing finer: canonical
// scopes run source, surface, data_subject, course..., and everything from
// data_subject on holds a household member or a course. Allowed shapes:
// `source=<v>/` and `source=<v>/surface=<v>/`.
func checkStalenessPrefix(p string) error {
	bad := func(why string) error {
		return fmt.Errorf("journal.staleness_prefixes: %q %s (want source=<value>/ or source=<value>/surface=<value>/)", p, why)
	}
	rest, ok := strings.CutSuffix(p, "/")
	if !ok {
		return bad("must end with '/'")
	}
	parts := strings.Split(rest, "/")
	if len(parts) > 2 {
		return bad("names more than a source and a surface")
	}
	for i, part := range parts {
		name, value, ok := strings.Cut(part, "=")
		if !ok || value == "" || strings.ContainsAny(value, "=") {
			return bad("has a part that is not name=<non-empty value>")
		}
		want := record.PartSource
		if i >= 1 {
			want = "surface"
		}
		if name != want {
			return bad("part " + strconv.Itoa(i+1) + " must be " + want + "=")
		}
	}
	return nil
}
