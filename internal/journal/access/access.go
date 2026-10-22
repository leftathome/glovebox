// Package access decides what a reader may see (spec 2026-10-16 4.8, 4.9,
// 4.11). It interprets no token: a row is visible when one of the reader's
// computed grants names the row's subject and one of its audience tokens,
// or when the audience contains "public".
package access

import (
	"encoding/json"

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

// Visible is the visibility rule. Precondition: grants belong to an
// authenticated reader who is present in the roster (an unknown reader has
// no grants, and the request layer answers it with 403 before this is
// called). A row whose audience contains "public" is visible to any such
// reader.
func Visible(grants []roster.Grant, subject string, audience []string) bool {
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
func Notices(grants []roster.Grant, subject string, in []rules.Notice) []rules.Notice {
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
