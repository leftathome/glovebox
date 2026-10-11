package powerschool

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// sourceName is the staging `source` and the match-key prefix.
const sourceName = "powerschool"

// Env carries what every surface processor needs. The Schoology processors
// take these as positional arguments; with three processors sharing eight
// of them a struct is easier to keep straight.
type Env struct {
	Client  PowerSchoolClient
	Writer  connector.StagingBackend
	Matcher *connector.RuleMatcher
	CP      connector.Checkpoint
	Tel     *Telemetry // may be nil
	TZ      *time.Location
	// Now is the clock; nil means time.Now. Injected so the assignment
	// lookback window is testable.
	Now func() time.Time
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Env) tz() *time.Location {
	if e.TZ != nil {
		return e.TZ
	}
	return time.UTC
}

// Result is what one surface processor did for one kid.
type Result struct {
	// Staged is the number of items committed.
	Staged int
	// Failed is the number of reads or commits that failed and were
	// skipped; the poll continued past them.
	Failed int
	// AuthErr is set when the library reported the session as expired or
	// rejected. The caller stops the poll and escalates.
	AuthErr error
}

func (r *Result) add(o Result) {
	r.Staged += o.Staged
	r.Failed += o.Failed
	if r.AuthErr == nil {
		r.AuthErr = o.AuthErr
	}
}

// MatchKey is the rule match key for a kid's surface:
// "powerschool:<kid>:<surface>".
func MatchKey(kid, surface string) string {
	return sourceName + ":" + kid + ":" + surface
}

// matchRule resolves the routing rule for a kid's surface. Without one the
// surface stages nothing: there is no fallback destination, because a
// child's school record must not be routed by accident.
func matchRule(e Env, kid Kid, surface string) (connector.MatchResult, bool) {
	key := MatchKey(kid.Name, surface)
	rule, ok := e.Matcher.Match(key)
	if !ok {
		slog.Warn("powerschool no rule match", "match_key", key, "kid", kid.Name)
		e.Tel.RecordItemDropped("no_rule")
	}
	return rule, ok
}

// errTeacherCommentAudience is returned by stage for a teacher_comment item
// whose audience is not exactly ["guardians"].
var errTeacherCommentAudience = errors.New("powerschool: teacher comment audience is not exactly [guardians]; not staged")

// stage commits one item under a powerschool.staging.commit span.
func stage(ctx context.Context, e Env, surface string, opts connector.ItemOptions, body string) error {
	_, span := e.Tel.StartSpan(ctx, "powerschool.staging.commit",
		attribute.String("surface", surface),
		attribute.String("destination", opts.DestinationAgent),
		attribute.String("data_subject", opts.DataSubject),
	)
	defer span.End()

	// The last line of the teacher-comment audience rule: whatever built
	// opts, a teacher comment leaves here for the guardians only.
	if surface == teacherCommentSurface && !guardiansOnly(opts.Audience) {
		return errTeacherCommentAudience
	}

	item, err := e.Writer.NewItem(opts)
	if err != nil {
		return err
	}
	if err := item.WriteContent([]byte(body)); err != nil {
		return err
	}
	if err := item.Commit(); err != nil {
		return err
	}
	e.Tel.RecordItemProduced(opts.DestinationAgent)
	return nil
}

// itemOptions fills the fields every powerschool item shares. tags must
// already carry the surface-specific keys; the common `kid`, `surface` and
// `change` keys are added here.
//
// The sender, the subject and every tag value are made one cleaned,
// bounded line here (oneLine), for every surface: they come from the
// portal's pages, and a line break or an invisible format character in a
// course name must not reach the metadata. A tag whose value has nothing
// visible in it is dropped, like any other unknown fact.
func itemOptions(e Env, rule connector.MatchResult, kid Kid, surface string, change Change, sender, subject string, tags map[string]string) connector.ItemOptions {
	sender, subject = oneLine(sender), oneLine(subject)
	if sender == "" {
		// Staging validation rejects an empty Sender.
		sender = "powerschool-course"
	}
	for k, v := range tags {
		if clean := oneLine(v); clean == "" {
			delete(tags, k)
		} else {
			tags[k] = clean
		}
	}
	tags["kid"] = kid.Name
	tags["surface"] = surface
	tags["change"] = change.String()
	return connector.ItemOptions{
		Source:           sourceName,
		Sender:           sender,
		Subject:          subject,
		Timestamp:        e.now().UTC(),
		DestinationAgent: rule.Destination,
		ContentType:      "text/plain",
		Tags:             tags,
		RuleTags:         rule.Tags,
		DataSubject:      rule.DataSubject,
		Audience:         rule.Audience,
	}
}

// courseKey identifies a course row for checkpointing. The portal's frn is
// stable for the enrolment; a row without one (no scores link yet) falls
// back to its name and period.
func courseKey(g *pslib.Grade) string {
	if g.CourseID != "" {
		return g.CourseID
	}
	return "name:" + strings.ToLower(strings.TrimSpace(g.CourseName)) + "|" + strings.TrimSpace(g.Period)
}

// setTag sets tags[key] only for a non-empty value, so absent facts are
// absent keys rather than empty strings.
func setTag(tags map[string]string, key, value string) {
	if v := strings.TrimSpace(value); v != "" {
		tags[key] = v
	}
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func formatBool(b bool) string {
	return strconv.FormatBool(b)
}
