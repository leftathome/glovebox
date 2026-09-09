package main

import (
	"encoding/json"
	"fmt"
)

// decodeEvents pulls the event list out of a controller response.
//
// Two shapes are accepted because the UDM serves both: the classic Network
// form wraps the list in {"data": [...]}, while Protect returns a bare array.
// Accepting both here means a firmware that changes which one it serves is a
// non-event rather than an outage.
func decodeEvents(body []byte) ([]json.RawMessage, error) {
	var wrapped struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Data != nil {
		return wrapped.Data, nil
	}

	var bare []json.RawMessage
	if err := json.Unmarshal(body, &bare); err == nil {
		return bare, nil
	}

	return nil, fmt.Errorf("response was neither {\"data\": [...]} nor a bare array")
}

// eventField returns the first of names present on the event as a non-empty
// string. Payload shapes differ between surfaces and firmware versions, so
// callers pass the aliases they are willing to accept.
func eventField(raw json.RawMessage, names []string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	for _, name := range names {
		v, ok := fields[name]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil && s != "" {
			return s
		}
		// Numeric ids appear in some payloads; render rather than discard.
		var n json.Number
		if err := json.Unmarshal(v, &n); err == nil && n.String() != "" {
			return n.String()
		}
	}
	return ""
}

// maxKindLen bounds a rendered event kind.
const maxKindLen = 64

// safeKind constrains an event kind to a conservative character set.
//
// The kind is a firmware enum (EVT_AP_RogueAp, smartDetectZone) and not a
// field a third party sets, so this is defence in depth rather than the
// primary control. It matters because the kind is interpolated into the
// subject line and into a rule lookup key: keeping it to an identifier shape
// means a payload that lied about its own type cannot reshape either. Free
// text still reaches the scanner through content.raw, which is where it
// belongs.
func safeKind(kind string) string {
	if kind == "" {
		return "unknown"
	}

	out := make([]byte, 0, len(kind))
	for i := 0; i < len(kind) && len(out) < maxKindLen; i++ {
		ch := kind[i]
		switch {
		case ch >= 'a' && ch <= 'z',
			ch >= 'A' && ch <= 'Z',
			ch >= '0' && ch <= '9',
			ch == '_', ch == '-', ch == '.':
			out = append(out, ch)
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}

// safeSubject builds the item subject from the surface name and the
// constrained kind only.
//
// Subject is a metadata channel that the engine scans, so putting attacker
// text here would not bypass anything. It is still built from enums alone:
// the subject is what a human sees first in a quarantine review, and it
// should describe the event rather than repeat an attacker's sentence back to
// the reviewer as if glovebox had written it.
func safeSubject(surfaceName, kind string) string {
	return fmt.Sprintf("unifi %s: %s", surfaceName, kind)
}
