// Package strictjson decodes security-relevant configuration (the journal's
// service policy and notification rules). encoding/json matches keys
// case-insensitively and lets the last duplicate win, so
// {"write_scopes":{...},"WRITE_SCOPES":null} silently drops a restriction.
// Decode refuses any object with duplicate keys or keys equal under
// strings.EqualFold, then refuses unknown fields, struct keys that are not
// exactly a declared json name, and trailing data.
//
// It deliberately refuses keys that differ only in case inside map values
// too, which is stricter than Go's case-sensitive map keys but fails closed.
package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Decode parses b into v strictly.
func Decode(b []byte, v any) error {
	if err := walk(json.NewDecoder(bytes.NewReader(b))); err != nil {
		return err
	}
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		return err
	}
	if err := checkExact(generic, reflect.TypeOf(v)); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("data after the JSON document")
	}
	return nil
}

// walk consumes one value from the token stream, rejecting duplicate keys.
func walk(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '[':
		for d.More() {
			if err := walk(d); err != nil {
				return err
			}
		}
	case '{':
		var seen []string
		for d.More() {
			kt, err := d.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			for _, p := range seen {
				if strings.EqualFold(p, key) {
					return fmt.Errorf("duplicate key %q (keys are compared without case)", key)
				}
			}
			seen = append(seen, key)
			if err := walk(d); err != nil {
				return err
			}
		}
	}
	_, err = d.Token() // closing delimiter
	return err
}
