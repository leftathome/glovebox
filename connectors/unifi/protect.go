package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/leftathome/glovebox/connector"
)

// Protect delivers events over a WebSocket subscription, not a REST list.
//
// There is no pollable events endpoint: GET /v1/events, /v1/subscribe/events
// over plain GET, /v1/cameras/{id}/events and the classic /proxy/protect/api/events
// all answer 404 or 401 on Protect 7.3.47. The official spec
// (developer.ui.com/protect/v7.3.47/openapi.json) documents
// GET /v1/subscribe/events as "A WebSocket subscription that broadcasts Protect
// events", and that is the only event transport there is.
//
// The subscription authenticates with the same read-only X-API-KEY used for the
// REST calls, so no session credential is required and the "eyes not hands"
// posture is unchanged. The upgrade MUST be attempted over HTTP/1.1: the UDM
// serves HTTP/2 by default and a WebSocket upgrade cannot be performed over it,
// which the controller reports as a 404 and which reads, wrongly, as a missing
// endpoint.
const protectSubscribePath = "/proxy/protect/integration/v1/subscribe/events"

// protectEnvelope is the frame shape: {"type":"add"|"update","item":{...}}.
type protectEnvelope struct {
	Type string          `json:"type"`
	Item json.RawMessage `json:"item"`
}

// protectItem is the subset of an event needed to aggregate it. Everything
// else in the payload is preserved verbatim and staged; this struct exists to
// find the identity and the lifecycle markers, not to model the event.
type protectItem struct {
	ID               string   `json:"id"`
	Type             string   `json:"type"`
	Start            *int64   `json:"start"`
	End              *int64   `json:"end"`
	Device           string   `json:"device"`
	SmartDetectTypes []string `json:"smartDetectTypes"`
}

// openEvent accumulates the frames belonging to one real-world event.
//
// Protect refines a detection while it is happening. A single person walking
// past produced, in a live capture on 2026-09-10:
//
//	add    smartDetectTypes ["person"]
//	update smartDetectTypes ["face","person"]
//	update smartDetectTypes ["face","person"]
//
// all carrying the same item.id, with `end` absent until the event closes.
// Staging each frame would deliver three items for one event, the first of them
// claiming a person was seen when a face was also identified a moment later.
// So frames are merged and the event is staged once, when it ends.
type openEvent struct {
	fields    map[string]json.RawMessage
	firstSeen time.Time
	lastSeen  time.Time
	frames    int
}

// protectWatcher owns the subscription and the open-event table.
type protectWatcher struct {
	c *UniFiConnector

	mu   sync.Mutex
	open map[string]*openEvent
}

func newProtectWatcher(c *UniFiConnector) *protectWatcher {
	return &protectWatcher{c: c, open: make(map[string]*openEvent)}
}

// maxOpenAge bounds how long an event may stay open before it is staged
// anyway.
//
// An event whose `end` never arrives -- because the subscription dropped, or
// because Protect simply never closed it -- would otherwise be held forever and
// never delivered. Staging it late and marked incomplete is better than
// silently dropping it.
const maxOpenAge = 5 * time.Minute

// Watch subscribes and blocks, delivering events until the context is
// cancelled or the connection fails. The framework's watch loop retries.
func (w *protectWatcher) Watch(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	url := "wss" + w.c.config.ControllerURL[len("https"):] + protectSubscribePath

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: w.c.httpClient,
		HTTPHeader: http.Header{
			"X-API-KEY":  []string{w.c.apiKey},
			"User-Agent": []string{connector.DefaultUserAgent},
		},
	})
	if err != nil {
		return fmt.Errorf("subscribe to protect events: %w", err)
	}
	defer conn.CloseNow()

	// Protect frames are small; a generous cap still refuses a hostile one.
	conn.SetReadLimit(maxEventBytes)

	logger.Info("subscribed to protect events", "url", protectSubscribePath)

	sweep := time.NewTicker(30 * time.Second)
	defer sweep.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sweep.C:
				w.flushStale(logger)
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read protect event: %w", err)
		}

		if err := w.handleFrame(data, checkpoint, logger); err != nil {
			// One malformed frame must not tear down the subscription.
			logger.Warn("skipping protect frame", "error", err)
		}
	}
}

// handleFrame merges one frame into the open-event table and stages the event
// if that frame closed it.
func (w *protectWatcher) handleFrame(data []byte, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	var env protectEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("parse envelope: %w", err)
	}
	if env.Type != "add" && env.Type != "update" {
		// Unknown envelope types are ignored rather than guessed at.
		return nil
	}
	if len(env.Item) == 0 {
		return fmt.Errorf("envelope %q carried no item", env.Type)
	}

	var item protectItem
	if err := json.Unmarshal(env.Item, &item); err != nil {
		return fmt.Errorf("parse item: %w", err)
	}
	if item.ID == "" {
		return fmt.Errorf("event carried no id")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(env.Item, &fields); err != nil {
		return fmt.Errorf("parse item fields: %w", err)
	}

	w.mu.Lock()
	ev, ok := w.open[item.ID]
	if !ok {
		ev = &openEvent{fields: make(map[string]json.RawMessage), firstSeen: time.Now()}
		w.open[item.ID] = ev
	}
	// Later frames win: an update carries the refined view.
	for k, v := range fields {
		ev.fields[k] = v
	}
	ev.lastSeen = time.Now()
	ev.frames++

	closed := item.End != nil
	if closed {
		delete(w.open, item.ID)
	}
	snapshot := ev
	w.mu.Unlock()

	if !closed {
		return nil
	}
	return w.stage(snapshot, item.ID, false, checkpoint, logger)
}

// flushStale stages events that have stayed open too long.
func (w *protectWatcher) flushStale(logger *slog.Logger) {
	cutoff := time.Now().Add(-maxOpenAge)

	w.mu.Lock()
	var stale []string
	for id, ev := range w.open {
		if ev.lastSeen.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	evs := make([]*openEvent, 0, len(stale))
	for _, id := range stale {
		evs = append(evs, w.open[id])
		delete(w.open, id)
	}
	w.mu.Unlock()

	for i, id := range stale {
		if err := w.stage(evs[i], id, true, nil, logger); err != nil {
			logger.Error("staging a stale protect event failed", "event", id, "error", err)
		}
	}
}

// stage writes one aggregated event to staging.
func (w *protectWatcher) stage(ev *openEvent, id string, incomplete bool, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	content, err := json.Marshal(ev.fields)
	if err != nil {
		return fmt.Errorf("marshal aggregated event: %w", err)
	}

	extra := map[string]string{
		"unifi.frames": fmt.Sprintf("%d", ev.frames),
	}
	if incomplete {
		// Say so rather than presenting a partial event as a whole one.
		extra["unifi.incomplete"] = "true"
	}

	if err := w.c.stageRaw(content, w.c.protectSurface(), "subscribe", extra, logger); err != nil {
		return err
	}
	if checkpoint != nil && id != "" {
		if err := checkpoint.Save("event:protect", id); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}
	return nil
}
