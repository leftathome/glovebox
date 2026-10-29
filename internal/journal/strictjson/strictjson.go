// Package strictjson decodes security-relevant configuration (the journal's
// service policy and notification rules) and the journal's request bodies.
// encoding/json matches keys case-insensitively and lets the last duplicate
// win, so {"write_scopes":{...},"WRITE_SCOPES":null} silently drops a
// restriction. Decode refuses any object with duplicate keys or keys equal
// under strings.EqualFold, then refuses unknown fields, struct keys that are
// not exactly a declared json name, and trailing data.
//
// It deliberately refuses keys that differ only in case inside map values
// too, which is stricter than Go's case-sensitive map keys but fails closed.
//
// The duplicate check is linear: each key is reduced to a canonical case
// fold (every rune to the smallest member of its Unicode simple-fold orbit,
// exactly the equivalence strings.EqualFold tests) and looked up in a set.
// DecodeLimits also caps the keys per object and per document, for input
// from the network.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode"
)

// ErrTooManyKeys: a document exceeds a key cap of DecodeLimits.
var ErrTooManyKeys = errors.New("strictjson: too many keys")

// Limits cap the keys of a document; zero is no cap.
type Limits struct {
	MaxObjectKeys int // keys in any one object
	MaxTotalKeys  int // keys in the whole document
}

// Decode parses b into v strictly, without key caps (configuration).
func Decode(b []byte, v any) error { return DecodeLimits(b, v, Limits{}) }

// DecodeLimits is Decode with key caps: a document over a cap fails with
// ErrTooManyKeys before it is decoded into anything.
func DecodeLimits(b []byte, v any, l Limits) error {
	w := &walker{d: json.NewDecoder(bytes.NewReader(b)), limits: l}
	if err := w.walk(); err != nil {
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

type walker struct {
	d      *json.Decoder
	limits Limits
	total  int
}

// walk consumes one value from the token stream, rejecting duplicate keys
// and enforcing the key caps.
func (w *walker) walk() error {
	tok, err := w.d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '[':
		for w.d.More() {
			if err := w.walk(); err != nil {
				return err
			}
		}
	case '{':
		seen := map[string]struct{}{}
		for w.d.More() {
			kt, err := w.d.Token()
			if err != nil {
				return err
			}
			w.total++
			if w.limits.MaxTotalKeys > 0 && w.total > w.limits.MaxTotalKeys {
				return fmt.Errorf("%w: more than %d in the document", ErrTooManyKeys, w.limits.MaxTotalKeys)
			}
			if w.limits.MaxObjectKeys > 0 && len(seen) >= w.limits.MaxObjectKeys {
				return fmt.Errorf("%w: more than %d in one object", ErrTooManyKeys, w.limits.MaxObjectKeys)
			}
			key, _ := kt.(string)
			f := foldKey(key)
			if _, dup := seen[f]; dup {
				return fmt.Errorf("duplicate key %q (keys are compared without case)", key)
			}
			seen[f] = struct{}{}
			if err := w.walk(); err != nil {
				return err
			}
		}
	}
	_, err = w.d.Token() // closing delimiter
	return err
}

// foldKey maps every rune to the smallest member of its simple case-fold
// orbit, so foldKey(a) == foldKey(b) exactly when strings.EqualFold(a, b).
// Invalid UTF-8 maps to U+FFFD, as EqualFold decodes it.
func foldKey(s string) string { return strings.Map(foldRune, s) }

func foldRune(r rune) rune {
	m := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < m {
			m = f
		}
	}
	return m
}
