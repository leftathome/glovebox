package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/leftathome/glovebox/internal/config"
)

// The route families served on the plaintext ingest plane. These are the
// three real paths, spelled out so a test failure names the endpoint an
// operator would notice going dark rather than an index.
const (
	routeIngest   = "/v1/ingest"
	routeArchives = "/v1/archives"
	routeSanitize = "/v1/sanitize"
)

// planFixture builds an IngestConfig for one cell of the tls-mode x
// bearer-split matrix. Auth is enabled throughout: it is the switch that
// decides whether the bearer surface exists at all, and every case here
// is about where those endpoints live, not whether they are configured.
func planFixture(mode string, bearerPort int) config.IngestConfig {
	return config.IngestConfig{
		Enabled:    true,
		Port:       9091,
		BearerPort: bearerPort,
		Auth:       config.IngestAuthConfig{Enabled: true},
		TLS: config.IngestTLSConfig{
			Mode: mode,
			Port: 9092,
		},
	}
}

// startPlan brings up a real httptest server per planned listener with
// stub handlers on each route family, and returns a lookup from port to
// server. Testing through net/http rather than asserting on the plan
// struct is deliberate: the regression being guarded is "the endpoint
// stopped answering", so the assertion should be an HTTP response.
func startPlan(t *testing.T, cfg config.IngestConfig) map[int]*httptest.Server {
	t.Helper()
	mountIngest := func(mux *http.ServeMux) {
		mux.HandleFunc(routeIngest, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}
	mountBearer := func(mux *http.ServeMux) error {
		// Mirrors the production mount: the archive listener registers a
		// subtree, the sanitize gate a single path.
		mux.HandleFunc(routeArchives+"/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mux.HandleFunc(routeArchives, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mux.HandleFunc(routeSanitize, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		return nil
	}

	servers := map[int]*httptest.Server{}
	for _, l := range planPlaintextListeners(cfg) {
		mux, err := buildPlaintextMux(l, mountIngest, mountBearer)
		if err != nil {
			t.Fatalf("buildPlaintextMux(:%d): %v", l.Port, err)
		}
		if _, dup := servers[l.Port]; dup {
			t.Fatalf("plan opens two listeners on the same port %d: %#v", l.Port, planPlaintextListeners(cfg))
		}
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		servers[l.Port] = srv
	}
	return servers
}

// get returns the status code the listener on port answers with, or 0 if
// no listener was opened on that port at all -- the failure mode this
// whole change exists to prevent.
func get(t *testing.T, servers map[int]*httptest.Server, port int, path string) int {
	t.Helper()
	srv, ok := servers[port]
	if !ok {
		return 0
	}
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET :%d%s: %v", port, path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestBearerEndpointsServedInEveryTLSMode is the regression test for the
// required-mode blackout: /v1/archives* and /v1/sanitize carry their own
// bearer auth (spec 10) and must not go offline because the connector
// transport moved to mTLS.
//
// Before this change the whole plaintext mux was gated on
// TLS.PlaintextActive(), so ingest.tls.mode=required opened no plaintext
// listener and both endpoints answered nothing at all. Under that
// behaviour every "required" subtest below fails with `got 0` -- no
// listener -- which is exactly the operator-visible symptom.
func TestBearerEndpointsServedInEveryTLSMode(t *testing.T) {
	for _, mode := range []string{config.TLSModeDisabled, config.TLSModePermissive, config.TLSModeRequired} {
		for _, split := range []struct {
			name       string
			bearerPort int
			wantPort   int
		}{
			{name: "shared-port", bearerPort: 0, wantPort: 9091},
			{name: "split-port", bearerPort: 9093, wantPort: 9093},
		} {
			t.Run(fmt.Sprintf("%s/%s", mode, split.name), func(t *testing.T) {
				cfg := planFixture(mode, split.bearerPort)
				servers := startPlan(t, cfg)

				for _, path := range []string{routeArchives, routeArchives + "/abc", routeSanitize} {
					if got := get(t, servers, split.wantPort, path); got != http.StatusOK {
						t.Errorf("GET :%d%s = %d, want %d (bearer endpoints must be served in tls mode %q; 0 means no listener was opened)",
							split.wantPort, path, got, http.StatusOK, mode)
					}
				}
			})
		}
	}
}

// TestIngestRouteFollowsTLSMode is the other half of the invariant: the
// bearer listener must not become a way around mTLS. Under required, no
// plaintext listener may answer /v1/ingest.
func TestIngestRouteFollowsTLSMode(t *testing.T) {
	cases := []struct {
		mode       string
		bearerPort int
		// wantIngest is the status /v1/ingest must return on the
		// plaintext ingest port. 0 means "no listener there".
		wantIngest int
	}{
		{config.TLSModeDisabled, 0, http.StatusOK},
		{config.TLSModeDisabled, 9093, http.StatusOK},
		{config.TLSModePermissive, 0, http.StatusOK},
		{config.TLSModePermissive, 9093, http.StatusOK},
		// required + shared: the listener on 9091 exists for the bearer
		// endpoints, but /v1/ingest is not registered on it, so it 404s.
		{config.TLSModeRequired, 0, http.StatusNotFound},
		// required + split: nothing is served on 9091 at all.
		{config.TLSModeRequired, 9093, 0},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/bearer-port-%d", tc.mode, tc.bearerPort), func(t *testing.T) {
			servers := startPlan(t, planFixture(tc.mode, tc.bearerPort))
			if got := get(t, servers, 9091, routeIngest); got != tc.wantIngest {
				t.Errorf("GET :9091%s = %d, want %d", routeIngest, got, tc.wantIngest)
			}
			// A split bearer listener must never carry /v1/ingest --
			// that is the whole point of granting the recognizer
			// namespace the bearer port instead of the ingest port.
			if tc.bearerPort != 0 {
				if got := get(t, servers, tc.bearerPort, routeIngest); got != http.StatusNotFound {
					t.Errorf("GET :%d%s = %d, want %d: the bearer listener must not serve the connector intake",
						tc.bearerPort, routeIngest, got, http.StatusNotFound)
				}
			}
		})
	}
}

// TestPlanPlaintextListeners pins the listener set itself, so a change to
// which ports are opened has to be made deliberately.
func TestPlanPlaintextListeners(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.IngestConfig
		want []plaintextListener
	}{
		{
			name: "ingest disabled opens nothing",
			cfg:  config.IngestConfig{Enabled: false, Port: 9091},
			want: nil,
		},
		{
			name: "no auth, tls disabled: one listener, ingest only",
			cfg:  config.IngestConfig{Enabled: true, Port: 9091},
			want: []plaintextListener{{Port: 9091, Ingest: true}},
		},
		{
			name: "no auth, tls required: nothing to serve in plaintext",
			cfg: config.IngestConfig{Enabled: true, Port: 9091,
				TLS: config.IngestTLSConfig{Mode: config.TLSModeRequired, Port: 9092}},
			want: nil,
		},
		{
			name: "auth, tls required, shared port: bearer surface only",
			cfg:  planFixture(config.TLSModeRequired, 0),
			want: []plaintextListener{{Port: 9091, Bearer: true}},
		},
		{
			name: "auth, tls permissive, shared port: both families, one listener",
			cfg:  planFixture(config.TLSModePermissive, 0),
			want: []plaintextListener{{Port: 9091, Ingest: true, Bearer: true}},
		},
		{
			name: "auth, tls disabled, split port: two listeners",
			cfg:  planFixture(config.TLSModeDisabled, 9093),
			want: []plaintextListener{{Port: 9091, Ingest: true}, {Port: 9093, Bearer: true}},
		},
		{
			name: "auth, tls required, split port: bearer listener only",
			cfg:  planFixture(config.TLSModeRequired, 9093),
			want: []plaintextListener{{Port: 9093, Bearer: true}},
		},
		{
			name: "bearer_port set to the ingest port is not a split",
			cfg:  planFixture(config.TLSModeDisabled, 9091),
			want: []plaintextListener{{Port: 9091, Ingest: true, Bearer: true}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := planPlaintextListeners(tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("planPlaintextListeners() = %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("listener %d = %#v, want %#v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
