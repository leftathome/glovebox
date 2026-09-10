package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

// The UDM is a trusted reporter of untrusted observations.
//
// Every field below is a string a third party can set, and none of them
// requires access to the network. A DHCP client names itself, so `hostname`
// is whatever the device claims. `essid` on a neighbouring or rogue access
// point is broadcast by radio, which means anyone within range can put text
// into a structurally valid, authenticated, TLS-verified event without ever
// associating. Protect's `licensePlate` is read off a physical object the
// observer does not control.
//
// This inventory is not a filter. Everything in an event reaches content.raw
// and therefore the scanner; the list exists so the item can be tagged with
// which untrusted channels were actually populated, and so a reader of this
// code learns the threat model without having to reconstruct it.
var untrustedNetworkFields = []string{
	"hostname",   // DHCP client hostname; device-chosen
	"name",       // client alias
	"essid",      // SSID, including neighbouring and rogue APs
	"ssid",       // ditto, older payload shape
	"msg",        // human-readable summary; interpolates hostname and SSID
	"ap_name",    // operator-set, but operator-set is not the same as fixed
	"sw_name",    //
	"gw_name",    //
	"network",    // network/VLAN display name
	"radio_name", //
}

var untrustedProtectFields = []string{
	"name",         // camera display name
	"licensePlate", // read off a physical plate the observer does not control
	"detectedName", // face-recognition label
	"description",  //
	"metadata",     // free-form bag; shape varies by detection type
}

// Media fields carry the bytes of a clip, a still or an audio segment, or a
// handle that resolves to them.
//
// Media does not transit glovebox. The scan engine is text pattern matching,
// so an h.264 clip or an AAC segment passes through it as opaque bytes that no
// rule can match, and it would emerge with a clean verdict having been checked
// by nothing. A green verdict on unscannable bytes is worse than an absent
// one, because downstream it is indistinguishable from a real result.
//
// The reference survives. A short handle or URL is left in place so an agent
// that genuinely needs the footage can reach for it deliberately, which is the
// same shape as TierFeed sending an item to caro for retrieval via
// search_items rather than pushing it into ambient recall.
var mediaFields = map[string]bool{
	"thumbnail": true,
	"heatmap":   true,
	"snapshot":  true,
	"image":     true,
	"preview":   true,
	"clip":      true,
	"audio":     true,
	"video":     true,
}

// mediaRefMaxLen is the longest string still treated as a reference rather
// than as an inlined payload. Protect identifiers and URLs sit far below it;
// a base64 still sits far above.
const mediaRefMaxLen = 256

// maxEventBytes bounds one event.
//
// Oversized events are skipped, not truncated. A truncated event looks
// complete while missing whatever came after the cut, and on a hostile input
// that is exactly where the interesting part would be -- the same reasoning
// the enrichment pipeline applies to enricher output.
const maxEventBytes = 1 << 20

// placeholder replaces an inlined media payload. It names what happened so a
// reader of the staged item is not left wondering whether the camera produced
// nothing or the field was removed here.
func placeholder(field string, n int) string {
	return fmt.Sprintf("<removed: %d bytes of inlined %s; media does not transit glovebox>", n, field)
}

// normalized is the result of preparing one raw event for staging.
type normalized struct {
	// Content is what gets written to content.raw. When no media was
	// stripped this is the original bytes, unmodified.
	Content []byte

	// StrippedMedia names the media fields whose payload was removed.
	StrippedMedia []string

	// PopulatedUntrusted names the untrusted free-text fields that were
	// present and non-empty.
	PopulatedUntrusted []string
}

// normalizeEvent prepares one raw event for staging.
//
// It returns the original bytes untouched when there is no inlined media,
// which keeps the common case byte-for-byte faithful to what the controller
// reported. It re-marshals only when something was actually removed.
func normalizeEvent(raw []byte, untrusted []string) (normalized, error) {
	if len(raw) > maxEventBytes {
		return normalized{}, fmt.Errorf("event is %d bytes, over the %d-byte limit", len(raw), maxEventBytes)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return normalized{}, fmt.Errorf("parse event: %w", err)
	}

	populated := populatedUntrusted(fields, untrusted)

	stripped, removed := stripMedia(fields)
	if len(removed) == 0 {
		// Nothing removed: hand back exactly what the controller sent.
		return normalized{
			Content:            raw,
			PopulatedUntrusted: populated,
		}, nil
	}

	content, err := json.Marshal(stripped)
	if err != nil {
		return normalized{}, fmt.Errorf("re-marshal event: %w", err)
	}

	return normalized{
		Content:            content,
		StrippedMedia:      removed,
		PopulatedUntrusted: populated,
	}, nil
}

// stripMedia replaces inlined media payloads with a placeholder, leaving
// references and structure in place. It reports which fields it changed.
//
// Only JSON *strings* longer than mediaRefMaxLen are treated as payloads. An
// object or an array under a media-named key is structure, not bytes: Protect
// carries detection geometry and per-detection metadata in exactly that shape,
// and a bounding box is the sort of thing this connector exists to deliver.
// Replacing it wholesale because its key happened to be "image" destroyed the
// most useful part of the event (glovebox-ext6).
//
// The walk recurses so that a blob nested inside metadata is still caught,
// while every level of structure around it survives.
func stripMedia(fields map[string]json.RawMessage) (map[string]json.RawMessage, []string) {
	var removed []string
	out := make(map[string]json.RawMessage, len(fields))
	for k, v := range fields {
		out[k] = stripValue(k, v, &removed)
	}
	sort.Strings(removed)
	return out, dedupe(removed)
}

// stripValue applies the payload rule to one value, recursing through
// containers. key is the name the value was reached under, which is what
// decides whether a long string is a media payload or ordinary long text.
func stripValue(key string, v json.RawMessage, removed *[]string) json.RawMessage {
	// A string under a media-named key: payload if long, reference if short.
	var str string
	if err := json.Unmarshal(v, &str); err == nil {
		if mediaFields[key] && len(str) > mediaRefMaxLen {
			repl, err := json.Marshal(placeholder(key, len(v)))
			if err != nil {
				return v
			}
			*removed = append(*removed, key)
			return repl
		}
		return v
	}

	// An object: recurse, keeping every key.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(v, &obj); err == nil {
		for k, vv := range obj {
			obj[k] = stripValue(k, vv, removed)
		}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return v
	}

	// An array: recurse, elements inheriting the parent key so that
	// "thumbnails": ["<blob>", ...] is still recognised.
	var arr []json.RawMessage
	if err := json.Unmarshal(v, &arr); err == nil {
		for i, vv := range arr {
			arr[i] = stripValue(key, vv, removed)
		}
		if out, err := json.Marshal(arr); err == nil {
			return out
		}
		return v
	}

	// Numbers, booleans, null: never payloads.
	return v
}

func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// populatedUntrusted reports which of the named untrusted fields are present
// and carry something. A field set to "" or null is not reported: the point
// is to say which hostile channels were actually used.
func populatedUntrusted(fields map[string]json.RawMessage, names []string) []string {
	var found []string
	for _, name := range names {
		v, ok := fields[name]
		if !ok || len(v) == 0 || string(v) == "null" || string(v) == `""` {
			continue
		}
		found = append(found, name)
	}
	sort.Strings(found)
	return found
}
