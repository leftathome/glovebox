package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// UniFiConnector delivers UDM Network and Protect events into glovebox.
//
// Both surfaces are one connector rather than two: they share a controller, a
// read-only credential, a staging backend and a tier, and splitting them would
// duplicate all four to gain nothing an operator wants.
type UniFiConnector struct {
	config        Config
	writer        connector.StagingBackend
	matcher       *connector.RuleMatcher
	fetchCounter  *connector.FetchCounter
	httpClient    *http.Client
	apiKey        string
	webhookSecret []byte
}

// surface describes one event source on the controller. The two surfaces
// differ only in where their events live, what the payload calls an id, and
// which fields are attacker-settable, so they are data rather than code.
type surface struct {
	name       string
	path       string
	idFields   []string
	kindFields []string
	untrusted  []string
	limit      int
}

func (c *UniFiConnector) networkSurface() surface {
	return surface{
		name:       "network",
		path:       fmt.Sprintf(c.config.Network.EventsPath, c.config.Site),
		idFields:   []string{"_id", "id"},
		kindFields: []string{"key", "type"},
		untrusted:  untrustedNetworkFields,
		limit:      c.config.Network.BackfillLimit,
	}
}

func (c *UniFiConnector) protectSurface() surface {
	return surface{
		name:       "protect",
		path:       c.config.Protect.EventsPath,
		idFields:   []string{"id", "_id"},
		kindFields: []string{"type", "key"},
		untrusted:  untrustedProtectFields,
		limit:      c.config.Protect.BackfillLimit,
	}
}

// Poll backfills from the controller API. It runs on start to catch up on
// whatever arrived while the connector was down, and on the poll interval
// thereafter as a safety net for webhook pushes that were never delivered.
func (c *UniFiConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	var surfaces []surface
	if c.config.Network.Enabled {
		surfaces = append(surfaces, c.networkSurface())
	}
	if c.config.Protect.Enabled {
		surfaces = append(surfaces, c.protectSurface())
	}
	if len(surfaces) == 0 {
		return connector.PermanentError(fmt.Errorf("neither the network nor the protect surface is enabled; this connector would do nothing"))
	}

	// One surface failing does not stop the other. A Protect outage should
	// not stop network events from arriving.
	for _, s := range surfaces {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollSurface(ctx, s, checkpoint, logger); err != nil {
			logger.Warn("surface poll failed", "surface", s.name, "error", err)
		}
	}
	return nil
}

func (c *UniFiConnector) pollSurface(ctx context.Context, s surface, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	body, err := c.fetchAPI(ctx, c.config.ControllerURL+s.path)
	if err != nil {
		return fmt.Errorf("fetch %s events: %w", s.name, err)
	}

	events, err := decodeEvents(body)
	if err != nil {
		return fmt.Errorf("decode %s events: %w", s.name, err)
	}
	if len(events) == 0 {
		return nil
	}

	cpKey := "event:" + s.name
	lastID, hasCheckpoint := checkpoint.Load(cpKey)

	// Resume after the last event we staged. When the checkpoint is not in
	// this page we have fallen further behind than the page reaches; the
	// backfill limit below bounds what that costs.
	startIdx := 0
	if hasCheckpoint {
		for i, ev := range events {
			if eventField(ev, s.idFields) == lastID {
				startIdx = i + 1
				break
			}
		}
	}
	if remaining := len(events) - startIdx; remaining > s.limit {
		skipped := remaining - s.limit
		logger.Warn("backfill limit reached; skipping oldest events",
			"surface", s.name, "skipped", skipped, "limit", s.limit)
		startIdx = len(events) - s.limit
	}

	for i := startIdx; i < len(events); i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if status := c.fetchCounter.TryFetch(s.name); !status.Allowed() {
			logger.Info("fetch limit reached, stopping", "surface", s.name, "status", status)
			break
		}

		id := eventField(events[i], s.idFields)
		if err := c.stageEvent(events[i], s, "poll", logger); err != nil {
			return fmt.Errorf("stage %s event %q: %w", s.name, id, err)
		}
		if id != "" {
			if err := checkpoint.Save(cpKey, id); err != nil {
				return fmt.Errorf("save checkpoint: %w", err)
			}
		}
	}
	return nil
}

// stageEvent normalises one event and writes it to staging.
//
// An event that cannot be normalised -- oversized, or not an object -- is
// logged and skipped rather than failing the whole poll. One malformed event
// among a page of good ones should not stop the good ones arriving.
func (c *UniFiConnector) stageEvent(raw json.RawMessage, s surface, via string, logger *slog.Logger) error {
	norm, err := normalizeEvent(raw, s.untrusted)
	if err != nil {
		logger.Warn("skipping event", "surface", s.name, "error", err)
		return nil
	}

	kind := safeKind(eventField(raw, s.kindFields))

	// Rules are looked up most specific first, so an operator can route one
	// noisy event kind differently without restating the rest.
	result, ok := c.matcher.Match("event:" + kind)
	if !ok {
		result, ok = c.matcher.Match(s.name)
	}
	if !ok {
		logger.Debug("no rule matched, not staging", "surface", s.name, "kind", kind)
		return nil
	}

	tags := map[string]string{
		"unifi.surface": s.name,
		"unifi.kind":    kind,
		"unifi.via":     via,
	}
	// Naming the hostile channels that were populated lets a reviewer of a
	// quarantined item see immediately which field to look at.
	if len(norm.PopulatedUntrusted) > 0 {
		tags["unifi.untrusted_fields"] = strings.Join(norm.PopulatedUntrusted, ",")
	}
	if len(norm.StrippedMedia) > 0 {
		tags["unifi.stripped_media"] = strings.Join(norm.StrippedMedia, ",")
	}

	item, err := c.writer.NewItem(connector.ItemOptions{
		Source:           "unifi",
		Sender:           "unifi-" + s.name,
		Subject:          safeSubject(s.name, kind),
		Ordered:          true,
		Timestamp:        time.Now().UTC(),
		DestinationAgent: result.Destination,
		// A narrow content type keeps the ruleset targeted at this payload
		// shape instead of every application/json item in the system.
		ContentType: "application/vnd.unifi.event+json",
		Tags:        tags,
		RuleTags:    result.Tags,
		Identity:    &connector.Identity{Provider: "unifi", AuthMethod: "api-key"},
	})
	if err != nil {
		return fmt.Errorf("new staging item: %w", err)
	}
	if err := item.WriteContent(norm.Content); err != nil {
		return fmt.Errorf("write content: %w", err)
	}
	if err := item.Commit(); err != nil {
		return fmt.Errorf("commit item: %w", err)
	}
	return nil
}

func (c *UniFiConnector) fetchAPI(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", connector.DefaultUserAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// A rejected key will be rejected on every retry.
		return nil, connector.PermanentError(fmt.Errorf("HTTP %d from %s: the API key was rejected", resp.StatusCode, url))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	const maxBody = 16 << 20
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}

// newHTTPClient builds the controller client.
//
// A UDM presents a self-signed certificate for its LAN address, so an
// operator has three options and all of them are explicit: supply the
// controller's certificate as a CA bundle, trust the system pool, or turn
// verification off and rely on the API key alone.
func newHTTPClient(cfg Config) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	switch {
	case cfg.CACertFile != "":
		pem, err := os.ReadFile(cfg.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_cert_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_cert_file %q contained no usable certificate", cfg.CACertFile)
		}
		tlsCfg.RootCAs = pool
	case cfg.InsecureSkipVerify:
		tlsCfg.InsecureSkipVerify = true
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg

	return &http.Client{Timeout: 30 * time.Second, Transport: transport}, nil
}
