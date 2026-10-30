package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// TestMain lets a test run the core itself: the test binary re-executed
// with GLOVEBOX_TEST_RUN_CORE=1 runs main() with the config in
// GLOVEBOX_TEST_CORE_CONFIG (and, for an enabled journal, the
// ServiceAccount files in GLOVEBOX_TEST_SA_DIR).
func TestMain(m *testing.M) {
	if os.Getenv("GLOVEBOX_TEST_RUN_CORE") == "1" {
		os.Args = []string{os.Args[0], "-config", os.Getenv("GLOVEBOX_TEST_CORE_CONFIG")}
		if d := os.Getenv("GLOVEBOX_TEST_SA_DIR"); d != "" {
			serviceAccountDir = d
		}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// syncBuffer is the child's output, written by exec's copier while the
// test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type core struct {
	cmd     *exec.Cmd
	out     *syncBuffer
	base    string
	metrics int
	done    chan error
	// exited is closed once Wait has returned. Cleanup checks it rather than
	// cmd.ProcessState, which Wait writes from another goroutine.
	exited chan struct{}
}

// coreConfig is a core config in base: ingest off, one agent, the default
// rules, metrics on a free port.
func coreConfig(t *testing.T, base string, metricsPort int, journal any) []byte {
	t.Helper()
	rules, err := filepath.Abs("configs/default-rules.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"staging_dir":           filepath.Join(base, "staging"),
		"quarantine_dir":        filepath.Join(base, "quarantine"),
		"audit_dir":             filepath.Join(base, "audit"),
		"failed_dir":            filepath.Join(base, "failed"),
		"agents_dir":            filepath.Join(base, "agents"),
		"shared_dir":            filepath.Join(base, "shared"),
		"agent_allowlist":       []string{"messaging"},
		"metrics_port":          metricsPort,
		"poll_interval_seconds": 1,
		"rules_file":            rules,
		"scan_workers":          2,
		"scan_timeout_seconds":  60,
		"ingest":                map[string]any{"enabled": false},
		"journal":               journal,
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func startCore(t *testing.T, cfg []byte, base string, metricsPort int, env ...string) *core {
	t.Helper()
	for _, d := range []string{"staging", "audit", "agents/messaging/workspace/inbox"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := writeFile(t, filepath.Join(base, "config.json"), string(cfg))
	c := &core{out: &syncBuffer{}, base: base, metrics: metricsPort, done: make(chan error, 1), exited: make(chan struct{})}
	c.cmd = exec.Command(os.Args[0])
	c.cmd.Env = append(os.Environ(), append([]string{"GLOVEBOX_TEST_RUN_CORE=1", "GLOVEBOX_TEST_CORE_CONFIG=" + path}, env...)...)
	c.cmd.Stdout, c.cmd.Stderr = c.out, c.out
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		c.done <- c.cmd.Wait()
		close(c.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-c.exited:
		default:
			_ = c.cmd.Process.Kill()
			<-c.exited
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(metricsPort) + "/readyz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return c
			}
		}
		select {
		case err := <-c.done:
			t.Fatalf("the core exited at start: %v\n%s", err, c.out)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the core never became ready\n%s", c.out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// deliver stages one clean item and waits for it in the agent's inbox.
func (c *core) deliver(t *testing.T, name string) {
	t.Helper()
	tmp := filepath.Join(c.base, "tmp-"+name)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmp, "content.raw"), "Hello, the meeting is at 3pm tomorrow.")
	writeFile(t, filepath.Join(tmp, "metadata.json"), `{"source": "email", "sender": "alice@example.com",
		"timestamp": "2026-03-28T12:00:00Z", "destination_agent": "messaging", "content_type": "text/plain"}`)
	if err := os.Rename(tmp, filepath.Join(c.base, "staging", name)); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(c.base, "agents", "messaging", "workspace", "inbox", name, "content.raw")
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(want); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("item %s not delivered\n%s", name, c.out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *core) scrape(t *testing.T) string {
	t.Helper()
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(c.metrics) + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// scrapeUntil scrapes until the output contains want. A counter appears in
// the exporter only after its first increment, which lands just after the
// item reaches the inbox: on a loaded CI runner a single scrape straight
// after deliver can miss it.
func (c *core) scrapeUntil(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		out := c.scrape(t)
		if strings.Contains(out, want) || time.Now().After(deadline) {
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop sends SIGTERM and requires a clean exit.
func (c *core) stop(t *testing.T) {
	t.Helper()
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatalf("the core exited with %v\n%s", err, c.out)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the core did not stop\n%s", c.out)
	}
	if !strings.Contains(c.out.String(), "glovebox stopped") {
		t.Errorf("no clean stop\n%s", c.out)
	}
}

func refused(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err == nil {
		conn.Close()
		return false
	}
	return true
}

// The journal off (the default): items flow, nothing journal-related is
// exported or listens, no database is dialled, and the core stops cleanly.
func TestCoreWithTheJournalOff(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the core")
	}
	base := t.TempDir()
	metricsPort, journalPort := closedPort(t), closedPort(t)
	cfg := coreConfig(t, base, metricsPort, map[string]any{"enabled": false, "port": journalPort,
		"database": map[string]any{"host": "db.invalid"}})
	c := startCore(t, cfg, base, metricsPort)
	c.deliver(t, "20260328-off")
	out := c.scrapeUntil(t, "glovebox_items_processed_total")
	if !strings.Contains(out, "glovebox_items_processed_total") {
		t.Fatalf("the scrape is not the core's:\n%s", out)
	}
	if strings.Contains(out, "glovebox_journal_") {
		t.Errorf("journal metrics with the journal off:\n%s", out)
	}
	if !refused(journalPort) {
		t.Errorf("something listens on the journal port %d with the journal off", journalPort)
	}
	c.stop(t)
	if log := c.out.String(); strings.Contains(log, "journal") {
		t.Errorf("the core logged about the journal with it off:\n%s", log)
	}
}

// selfSignedPEM is a CA certificate for the fake ServiceAccount directory.
func selfSignedPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// The journal on but degraded -- its database unreachable, or its roster
// missing as well: the core starts and is ready, items flow, the journal
// answers 503 unavailable, exports ready 0 and keeps retrying, and the core
// stops cleanly.
func TestCoreWithTheJournalDegraded(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the core")
	}
	for _, tc := range []struct {
		name          string
		rosterPresent bool
		failing       float64
		retry         string // what the retry log line names
	}{
		{"database unreachable", true, 0, "failed to connect"},
		{"database unreachable, roster missing", false, 1, "roster: open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			metricsPort, journalPort := closedPort(t), closedPort(t)
			rosterPath := filepath.Join(base, "household.json")
			if tc.rosterPresent {
				rosterPath = rosterFile(t)
			}
			jc := testJournalConfig(t, unreachableDB(t), rosterPath)
			jc.Port = journalPort
			jc.Database.SSLMode = "require" // the validator refuses anything weaker
			sa := t.TempDir()
			writeFile(t, filepath.Join(sa, "ca.crt"), selfSignedPEM(t))
			writeFile(t, filepath.Join(sa, "namespace"), "openclaw\n")
			writeFile(t, filepath.Join(sa, "token"), "own-token\n")
			cfg := coreConfig(t, base, metricsPort, jc)
			c := startCore(t, cfg, base, metricsPort, "GLOVEBOX_TEST_SA_DIR="+sa,
				"KUBERNETES_SERVICE_HOST=127.0.0.1", "KUBERNETES_SERVICE_PORT="+strconv.Itoa(closedPort(t)))
			c.deliver(t, "20260328-degraded")

			resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(journalPort) + journalapi.PathHealthz)
			if err != nil {
				t.Fatalf("journal listener: %v\n%s", err, c.out)
			}
			var ref journalapi.Response
			_ = json.NewDecoder(resp.Body).Decode(&ref)
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable || ref.Reason != journalapi.ReasonUnavailable {
				t.Errorf("degraded journal: %d %+v", resp.StatusCode, ref)
			}
			// Wait for a retry that names the cause (after the scanner warm-up).
			deadline := time.Now().Add(60 * time.Second)
			for !strings.Contains(c.out.String(), "journal not ready: ") || !strings.Contains(c.out.String(), tc.retry) {
				if time.Now().After(deadline) {
					t.Fatalf("no retry naming %q:\n%s", tc.retry, c.out)
				}
				time.Sleep(50 * time.Millisecond)
			}
			_, sha := testScanner(t)
			if want := "scanner " + jc.ScannerVersion + "+rules." + sha[:12]; !strings.Contains(c.out.String(), want) {
				t.Errorf("the journal does not use %q:\n%s", want, c.out)
			}
			c.deliver(t, "20260328-degraded-2")
			out := c.scrape(t)
			if v, ok := metricValue(out, "glovebox_journal_ready"); !ok || v != 0 {
				t.Errorf("ready %v %v", v, ok)
			}
			if v, ok := metricValue(out, "glovebox_journal_roster_reload_failing"); !ok || v != tc.failing {
				t.Errorf("roster_reload_failing %v %v, want %v", v, ok, tc.failing)
			}
			if !strings.Contains(out, "glovebox_items_processed_total") {
				t.Error("no item metrics")
			}
			resp, err = http.Get("http://127.0.0.1:" + strconv.Itoa(metricsPort) + "/readyz")
			if err != nil || resp.StatusCode != 200 {
				t.Errorf("the core's readiness follows the journal: %v %v", err, resp)
			}
			if resp != nil {
				resp.Body.Close()
			}
			c.stop(t)
			if log := c.out.String(); !strings.Contains(log, "journal stopped") ||
				strings.Index(log, "journal stopped") > strings.Index(log, "glovebox stopped") {
				t.Errorf("the journal was not shut down before the core stopped:\n%s", log)
			}
		})
	}
}
