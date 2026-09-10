package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

func sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, c *UniFiConnector, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)
	return rec
}

const webhookBody = `{"id":"w1","type":"motion","name":"front-door"}`

func TestWebhook_ValidSignatureStagesWithFeedTier(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	rec := postWebhook(t, c, "/protect", webhookBody, map[string]string{
		"X-Unifi-Signature": sign(c.webhookSecret, []byte(webhookBody)),
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body = %s", rec.Code, rec.Body.String())
	}
	items := readStaged(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("expected 1 staged item, got %d", len(items))
	}
	if items[0].Meta.Tier != string(connector.TierFeed) {
		t.Errorf("tier = %q, want feed", items[0].Meta.Tier)
	}
	if items[0].Meta.Tags["unifi.via"] != "webhook" {
		t.Errorf("via tag = %q, want webhook", items[0].Meta.Tags["unifi.via"])
	}
}

// A forged push must not reach staging. Staging is a write channel into agent
// context; this is the control that keeps it closed.
func TestWebhook_BadSignatureIsRejected(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	rec := postWebhook(t, c, "/protect", webhookBody, map[string]string{
		"X-Unifi-Signature": sign([]byte("wrong-secret"), []byte(webhookBody)),
	})

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := len(readStaged(t, stagingDir)); got != 0 {
		t.Errorf("a forged webhook staged %d items; it must stage none", got)
	}
}

func TestWebhook_MissingSignatureIsRejected(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	rec := postWebhook(t, c, "/protect", webhookBody, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if got := len(readStaged(t, stagingDir)); got != 0 {
		t.Errorf("expected nothing staged, got %d", got)
	}
}

// Fail closed: with no secret the listener refuses rather than accepting
// unverified pushes.
func TestWebhook_NoSecretConfiguredFailsClosed(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())
	c.webhookSecret = nil

	rec := postWebhook(t, c, "/protect", webhookBody, map[string]string{
		"X-Unifi-Signature": "anything",
	})

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := len(readStaged(t, stagingDir)); got != 0 {
		t.Errorf("expected nothing staged, got %d", got)
	}
}

func TestWebhook_BearerMode(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL:   "https://unifi.example",
		Protect:         ProtectConfig{Enabled: true},
		WebhookAuthMode: authModeBearer,
	}, allowAll())

	t.Run("correct secret", func(t *testing.T) {
		rec := postWebhook(t, c, "/protect", webhookBody, map[string]string{
			"Authorization": "Bearer " + string(c.webhookSecret),
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200. body = %s", rec.Code, rec.Body.String())
		}
		if got := len(readStaged(t, stagingDir)); got != 1 {
			t.Errorf("expected 1 staged item, got %d", got)
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		rec := postWebhook(t, c, "/protect", webhookBody, map[string]string{
			"Authorization": "Bearer not-the-secret",
		})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
	})
}

// A push to a surface the operator did not enable is not a route that exists.
func TestWebhook_DisabledSurfaceIsNotFound(t *testing.T) {
	c, _ := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	rec := postWebhook(t, c, "/network", webhookBody, map[string]string{
		"X-Unifi-Signature": sign(c.webhookSecret, []byte(webhookBody)),
	})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestWebhook_GETIsRejected(t *testing.T) {
	c, _ := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	req := httptest.NewRequest(http.MethodGet, "/protect", nil)
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// An inlined still arriving by webhook is removed the same way it is on the
// poll path.
func TestWebhook_InlinedMediaIsStripped(t *testing.T) {
	blob := strings.Repeat("Q", 4096)
	body := `{"id":"w2","type":"motion","thumbnail":"` + blob + `"}`

	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())

	rec := postWebhook(t, c, "/protect", body, map[string]string{
		"X-Unifi-Signature": sign(c.webhookSecret, []byte(body)),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	items := readStaged(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("expected 1 staged item, got %d", len(items))
	}
	if strings.Contains(string(items[0].Content), blob) {
		t.Error("inlined media reached staging; media must not transit glovebox")
	}
	if items[0].Meta.Tags["unifi.stripped_media"] != "thumbnail" {
		t.Errorf("stripped_media tag = %q, want thumbnail", items[0].Meta.Tags["unifi.stripped_media"])
	}
}
