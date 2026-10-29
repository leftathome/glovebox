// Package access decides what a reader may see (spec 2026-10-16 4.8, 4.9,
// 4.11). It interprets no token: a row is visible when one of the reader's
// computed grants names the row's subject and one of its audience tokens,
// or when the audience contains "public".
package access

import (
	"encoding/json"
	"strings"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

// Public in an audience makes a row visible to every authenticated reader.
const Public = "public"

// Withheld markers, returned in place of values (4.8, 4.11).
const (
	WithheldScan     = journalapi.WithheldPrefix + "failed content scan"
	WithheldAudience = journalapi.WithheldPrefix + "different audience"
)

var (
	withheldScanJSON     = record.Encode(WithheldScan)
	withheldAudienceJSON = record.Encode(WithheldAudience)
)

// ErasedPrefix starts the data_subject of a note kept when its subject was
// erased (spec 4.13): "erased:<pseudonym>:<n>". Roster ids cannot contain
// ':', so no roster member, and no roster grant, ever names one.
const ErasedPrefix = "erased:"

// Frozen-reader grants (spec 4.13). For every kept note s that the table
// erasure_reader lists a reader for, the journal adds one grant to the
// reader's grants: (s, ErasedAuthor) for the note's author, and
// (s, ReaderToken(frozen)) for any other frozen reader, frozen being the
// audience the readers were computed from at erase time. Both tokens
// contain ':', so no type or snapshot can name them as audience tokens.
const (
	ErasedAuthor       = "erased:author"
	ErasedReaderPrefix = "erased:reader:"
)

// ReaderToken is the frozen-reader token for readers frozen from audience
// (sorted, distinct tokens, as stored; audience tokens contain no ',').
func ReaderToken(audience []string) string { return ErasedReaderPrefix + strings.Join(audience, ",") }

// IsErased reports whether subject is a kept note's pseudonymous subject.
func IsErased(subject string) bool { return strings.HasPrefix(subject, ErasedPrefix) }

// Visible is the visibility rule. Precondition: grants belong to an
// authenticated reader who is present in the roster and not erased (an
// unknown or erased reader has no grants, and the request layer answers it
// with 403 before this is called), plus the reader's frozen-reader grants.
//
// A kept note (IsErased(subject)) takes one branch: roster grants and
// "public" never reach it. Its author sees every row of it, whatever the
// row's audience. Any other frozen reader sees a row (the note, an event, or
// an old value across an audience change, through OldValue) only when that
// row's audience is exactly the audience the readers were frozen from;
// anything else is withheld, as for a different audience. Any other row is
// visible when its audience contains "public", or when a grant names its
// subject and one of its audience tokens; frozen-reader grants name only
// kept notes, so they never match one.
func Visible(grants []roster.Grant, subject string, audience []string) bool {
	if IsErased(subject) {
		reader := ReaderToken(audience)
		for _, g := range grants {
			if g.Subject == subject && (g.Token == ErasedAuthor || g.Token == reader) {
				return true
			}
		}
		return false
	}
	for _, t := range audience {
		if t == Public {
			return true
		}
	}
	for _, g := range grants {
		if g.Subject != subject {
			continue
		}
		for _, t := range audience {
			if g.Token == t {
				return true
			}
		}
	}
	return false
}

// Pairs splits grants into parallel subject and token arrays: the shape the
// SQL visibility clause unnests (Task 19).
func Pairs(grants []roster.Grant) (subjects, tokens []string) {
	subjects = make([]string, len(grants))
	tokens = make([]string, len(grants))
	for i, g := range grants {
		subjects[i], tokens[i] = g.Subject, g.Token
	}
	return subjects, tokens
}

// Text returns a stored text value as a reader may see it: the value when
// its own verdict is pass, the marker otherwise -- including when the
// verdict is missing (fail closed). Null stays null.
func Text(v json.RawMessage, verdict string) json.RawMessage {
	if v == nil {
		return nil
	}
	if verdict == record.VerdictPass {
		return v
	}
	return withheldScanJSON
}

// OldValue returns an old value of an updated event as a reader may see it.
// audienceChanged says the event changed the audience (prev_audience is not
// NULL) and prev is the audience before the change. When it changed, only a
// reader matching the old audience as well sees the old value (the event
// itself is visible only to readers of the new one). prev is not consulted
// when audienceChanged is false; an empty prev with audienceChanged true
// matches nobody, so it never releases the value (fail closed).
func OldValue(grants []roster.Grant, subject string, prev []string, audienceChanged bool, v json.RawMessage) json.RawMessage {
	if !audienceChanged || Visible(grants, subject, prev) {
		return v
	}
	return withheldAudienceJSON
}

// Notices keeps the notices whose token the reader holds for subject.
// A kept note's notices were advice to readers of a subject who is gone:
// nobody gets them.
func Notices(grants []roster.Grant, subject string, in []rules.Notice) []rules.Notice {
	if IsErased(subject) {
		return nil
	}
	var out []rules.Notice
	for _, n := range in {
		for _, g := range grants {
			if g.Subject == subject && g.Token == n.Token {
				out = append(out, n)
				break
			}
		}
	}
	return out
}
