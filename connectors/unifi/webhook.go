package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/leftathome/glovebox/connector"
)

// maxWebhookBody bounds an inbound push.
const maxWebhookBody = 4 << 20

var errBodyTooLarge = errors.New("webhook body exceeded the size limit")

// Handler implements connector.Listener: it receives UniFi webhook pushes
// once the initial backfill has caught up.
//
// The listener is fail-closed. With no secret configured it refuses every
// request, because a connector's staging directory is a write channel into
// agent context and an unauthenticated endpoint on it would let anyone who
// can reach the pod put content in front of an agent. That is a stricter
// posture than "accept when unconfigured", and it is deliberate: the failure
// mode of being too strict is a 503 in a log, and the failure mode of being
// too lax is undetectable.
func (c *UniFiConnector) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/network", c.surfaceHandler(c.networkSurface(), func() bool { return c.config.Network.Enabled }))
	mux.Handle("/protect", c.surfaceHandler(c.protectSurface(), func() bool { return c.config.Protect.Enabled }))
	return mux
}

func (c *UniFiConnector) surfaceHandler(s surface, enabled func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := slog.Default()

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !enabled() {
			http.NotFound(w, r)
			return
		}
		if len(c.webhookSecret) == 0 {
			logger.Warn("refusing webhook: no secret configured", "surface", s.name)
			http.Error(w, "webhook authentication is not configured", http.StatusServiceUnavailable)
			return
		}

		body, err := readLimited(r, maxWebhookBody)
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		if !c.authenticate(r, body) {
			logger.Warn("rejecting webhook: authentication failed", "surface", s.name)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		events, err := decodeWebhook(body)
		if err != nil {
			http.Error(w, "malformed payload", http.StatusBadRequest)
			return
		}

		for _, ev := range events {
			if err := c.stageEvent(ev, s, "webhook", logger); err != nil {
				logger.Error("staging webhook event failed", "surface", s.name, "error", err)
				http.Error(w, "staging error", http.StatusInternalServerError)
				return
			}
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// authenticate verifies an inbound push under the configured mode.
func (c *UniFiConnector) authenticate(r *http.Request, body []byte) bool {
	presented := r.Header.Get(c.config.WebhookSignatureHeader)
	if presented == "" {
		return false
	}

	switch c.config.WebhookAuthMode {
	case authModeBearer:
		presented = strings.TrimPrefix(presented, "Bearer ")
		// Compare through HMAC rather than comparing the secrets directly:
		// it is constant time and it does not leak the secret's length.
		return hmac.Equal(tag(c.webhookSecret, []byte(presented)), tag(c.webhookSecret, c.webhookSecret))
	case authModeHMAC:
		return connector.VerifyHMAC(body, presented, c.webhookSecret, "sha256")
	default:
		return false
	}
}

func tag(key, msg []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return mac.Sum(nil)
}

// decodeWebhook accepts a list of events or a single event object. UniFi
// posts one event per request on some surfaces and a batch on others.
func decodeWebhook(body []byte) ([]json.RawMessage, error) {
	if events, err := decodeEvents(body); err == nil {
		return events, nil
	}

	var single json.RawMessage
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, err
	}
	// Reject anything that is not a JSON object; normalizeEvent would fail
	// on it anyway, and failing here gives the sender a 400 instead of a
	// silent no-op.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(single, &probe); err != nil {
		return nil, err
	}
	return []json.RawMessage{single}, nil
}

// readLimited reads at most max bytes and reports an error if the body is
// larger, rather than truncating it. A truncated payload would parse as
// malformed at best and as a different event at worst.
func readLimited(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, errBodyTooLarge
	}
	return body, nil
}
