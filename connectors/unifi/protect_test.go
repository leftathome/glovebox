package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// Frames captured from the live subscription on 2026-09-10. One person walking
// past a camera produced three frames under a single item.id, with the
// detection refining from "person" to "face","person" and `end` absent
// throughout. The fourth is a separate motion event on another camera.
const (
	frameAdd    = `{"type":"add","item":{"id":"c0b8c646","modelKey":"event","type":"smartDetectZone","start":1789001974452,"device":"699fe724","smartDetectTypes":["person"]}}`
	frameUpdate = `{"type":"update","item":{"id":"c0b8c646","type":"smartDetectZone","start":1789001974452,"device":"699fe724","smartDetectTypes":["face","person"],"modelKey":"event"}}`
	frameEnd    = `{"type":"update","item":{"id":"c0b8c646","type":"smartDetectZone","start":1789001974452,"end":1789001999000,"device":"699fe724","smartDetectTypes":["face","person"],"modelKey":"event"}}`
	frameMotion = `{"type":"add","item":{"id":"23e10ecd","modelKey":"event","type":"motion","start":1789002063195,"device":"6736859600ff"}}`
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Three frames for one real-world event must produce exactly one staged item,
// carrying the refined detection list rather than the first partial one.
func TestProtectWatcher_AggregatesFramesIntoOneItem(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())
	w := newProtectWatcher(c)

	for _, f := range []string{frameAdd, frameUpdate, frameEnd} {
		if err := w.handleFrame([]byte(f), nil, quietLogger()); err != nil {
			t.Fatalf("handleFrame: %v", err)
		}
	}

	items := readStaged(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("three frames for one event staged %d items, want 1", len(items))
	}

	var got map[string]any
	if err := json.Unmarshal(items[0].Content, &got); err != nil {
		t.Fatalf("staged content is not an object: %v", err)
	}
	types, _ := json.Marshal(got["smartDetectTypes"])
	if string(types) != `["face","person"]` {
		t.Errorf("smartDetectTypes = %s, want the refined [\"face\",\"person\"]", types)
	}
	if got["end"] == nil {
		t.Error("the staged event should be the closed one, carrying end")
	}
	if items[0].Meta.Tags["unifi.frames"] != "3" {
		t.Errorf("frames tag = %q, want 3", items[0].Meta.Tags["unifi.frames"])
	}
}

// An event that has not ended is held, not staged: staging it would deliver a
// detection list that is still being refined.
func TestProtectWatcher_OpenEventIsNotStagedYet(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())
	w := newProtectWatcher(c)

	for _, f := range []string{frameAdd, frameUpdate, frameMotion} {
		if err := w.handleFrame([]byte(f), nil, quietLogger()); err != nil {
			t.Fatalf("handleFrame: %v", err)
		}
	}

	if got := len(readStaged(t, stagingDir)); got != 0 {
		t.Errorf("staged %d items while every event was still open, want 0", got)
	}
	if len(w.open) != 2 {
		t.Errorf("open events = %d, want 2", len(w.open))
	}
}

// An event whose end never arrives is staged late and labelled, rather than
// held forever.
func TestProtectWatcher_StaleEventIsFlushedAndLabelled(t *testing.T) {
	c, stagingDir := newTestConnector(t, Config{
		ControllerURL: "https://unifi.example",
		Protect:       ProtectConfig{Enabled: true},
	}, allowAll())
	w := newProtectWatcher(c)

	if err := w.handleFrame([]byte(frameAdd), nil, quietLogger()); err != nil {
		t.Fatal(err)
	}
	// Age it past the cutoff.
	w.mu.Lock()
	for _, ev := range w.open {
		ev.lastSeen = ev.lastSeen.Add(-2 * maxOpenAge)
	}
	w.mu.Unlock()

	w.flushStale(quietLogger())

	items := readStaged(t, stagingDir)
	if len(items) != 1 {
		t.Fatalf("stale event staged %d items, want 1", len(items))
	}
	if items[0].Meta.Tags["unifi.incomplete"] != "true" {
		t.Errorf("a flushed open event must be marked incomplete, tags = %v", items[0].Meta.Tags)
	}
}

// glovebox-ext6: structured detection data under a media-named key must
// survive. Asserted by parsing, not by substring: the previous version of this
// check passed because the placeholder text contains the word "glovebox",
// which contains "box".
func TestNormalizeEvent_StructureUnderMediaKeySurvives(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"id":    "evt1",
		"type":  "smartDetectZone",
		"image": map[string]any{"box": []int{10, 20, 100, 200}},
	})

	got, err := normalizeEvent(raw, untrustedProtectFields)
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}

	var out struct {
		Image struct {
			Box []int `json:"box"`
		} `json:"image"`
	}
	if err := json.Unmarshal(got.Content, &out); err != nil {
		t.Fatalf("content did not parse: %v\ncontent = %s", err, got.Content)
	}
	want := []int{10, 20, 100, 200}
	if len(out.Image.Box) != len(want) {
		t.Fatalf("bounding box was destroyed: got %v, want %v\ncontent = %s", out.Image.Box, want, got.Content)
	}
	for i := range want {
		if out.Image.Box[i] != want[i] {
			t.Errorf("box[%d] = %d, want %d", i, out.Image.Box[i], want[i])
		}
	}
	if len(got.StrippedMedia) != 0 {
		t.Errorf("structure is not a payload; stripped = %v", got.StrippedMedia)
	}
}

// The payload rule still applies: a long string under a media key goes.
func TestNormalizeEvent_NestedBlobStillStripped(t *testing.T) {
	blob := strings.Repeat("A", 4096)
	raw := mustJSON(t, map[string]any{
		"id": "evt1",
		"metadata": map[string]any{
			"detection": map[string]any{
				"box":       []int{1, 2, 3, 4},
				"thumbnail": blob,
			},
		},
	})

	got, err := normalizeEvent(raw, untrustedProtectFields)
	if err != nil {
		t.Fatalf("normalizeEvent: %v", err)
	}

	var out struct {
		Metadata struct {
			Detection struct {
				Box       []int  `json:"box"`
				Thumbnail string `json:"thumbnail"`
			} `json:"detection"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(got.Content, &out); err != nil {
		t.Fatalf("content did not parse: %v", err)
	}
	if len(out.Metadata.Detection.Box) != 4 {
		t.Errorf("nested box was destroyed: %v", out.Metadata.Detection.Box)
	}
	if strings.Contains(out.Metadata.Detection.Thumbnail, blob) {
		t.Error("nested media blob survived; media must not transit glovebox")
	}
	if !contains(got.StrippedMedia, "thumbnail") {
		t.Errorf("nested thumbnail should be reported stripped, got %v", got.StrippedMedia)
	}
}
