package connector

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A stock http.Client follows redirects blindly and re-resolves DNS at dial
// time. These tests pin the two behaviours that closes.

func TestGuardedClient_RefusesRedirectToInternalAddress(t *testing.T) {
	// The classic shape: a public-looking page 302s to the cloud metadata
	// service. Before the guard, the redirect was followed and the body was
	// delivered as "linked page" content.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/iam/security-credentials/", http.StatusFound)
	}))
	defer srv.Close()

	client := NewGuardedHTTPClient(GuardedClientOptions{
		AllowPrivateNetworks: true, // allow reaching the test server itself
		ValidateURL: func(rawURL string) error {
			if strings.Contains(rawURL, "169.254.169.254") {
				return errors.New("denied by policy")
			}
			return nil
		},
	})

	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected redirect to metadata address to be refused, got a response")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error = %v, want a redirect-refused error", err)
	}
}

func TestGuardedClient_CapsRedirectChain(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	client := NewGuardedHTTPClient(GuardedClientOptions{
		AllowPrivateNetworks: true,
		MaxRedirects:         3,
	})
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected the redirect loop to be capped")
	}
	if !strings.Contains(err.Error(), "stopped after 3 redirects") {
		t.Errorf("error = %v, want a redirect cap error", err)
	}
}

// The dialer must reject on the address it actually connects to, not on a
// name it resolved earlier. This is what defeats DNS rebinding: a resolver
// that returns a private address at dial time is refused even though the
// policy check saw something else.
func TestGuardedDialer_RejectsBlockedAddresses(t *testing.T) {
	g := &guardedDialer{dialer: &net.Dialer{}}

	blocked := []string{
		"127.0.0.1:80",
		"169.254.169.254:80",    // cloud metadata
		"10.0.0.5:80",           // RFC1918
		"192.168.1.1:80",        // RFC1918
		"172.16.0.1:80",         // RFC1918
		"[::1]:80",              // IPv6 loopback
		"[::ffff:127.0.0.1]:80", // IPv4-mapped loopback
		"100.64.0.1:80",         // CGNAT / overlay
		"0.0.0.0:80",            // unspecified
	}
	for _, addr := range blocked {
		t.Run(addr, func(t *testing.T) {
			conn, err := g.DialContext(context.Background(), "tcp", addr)
			if err == nil {
				conn.Close()
				t.Fatalf("DialContext(%s) succeeded, want blocked", addr)
			}
			var blockedErr *ErrBlockedAddress
			if !errors.As(err, &blockedErr) {
				t.Errorf("error = %v (%T), want *ErrBlockedAddress", err, err)
			}
		})
	}
}

func TestGuardedDialer_AllowsPrivateWhenOptedIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := NewGuardedHTTPClient(GuardedClientOptions{AllowPrivateNetworks: true})
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("opted-in private fetch failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// The guard is the whole point of the client: with it on, the loopback test
// server is unreachable even though the URL is syntactically fine.
func TestGuardedClient_BlocksLoopbackByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("should never be read"))
	}))
	defer srv.Close()

	client := NewGuardedHTTPClient(GuardedClientOptions{})
	resp, err := client.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("guarded client reached a loopback server")
	}
	var blockedErr *ErrBlockedAddress
	if !errors.As(err, &blockedErr) {
		t.Errorf("error = %v, want *ErrBlockedAddress", err)
	}
}
