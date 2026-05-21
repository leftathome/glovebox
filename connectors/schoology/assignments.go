package schoology

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
	schoologylib "github.com/leftathome/schoology-go"
)

// assignmentSurface is the per-kid checkpoint surface for assignments.
const assignmentSurface = "assignment"

// ProcessAssignments polls one kid's overdue/upcoming submissions and
// stages every new assignment as its own item.
//
// Returns:
//   - staged: number of assignments staged in this call.
//   - errsLogged: true if any parse-failure receipt was emitted (or a
//     top-level library error occurred). Used by callers feeding the
//     schema-drift counter (spec 12 §11.4).
//
// Per-assignment failures (checkpoint read, stage failure, no rule
// match for the assignment item) are logged and skipped; the poll
// continues. Top-level GetOverdueSubmissions errors emit a single
// parse-failure receipt and short-circuit with staged=0.
func ProcessAssignments(
	ctx context.Context,
	client SchoologyClient,
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	cp connector.Checkpoint,
	dedup *ReceiptDedup,
	kid Kid,
	libVersion string,
) (staged int, errsLogged bool) {
	assignments, parseErrs, err := client.GetOverdueSubmissions(ctx, kid.SchoologyUID)
	if err != nil {
		errsLogged = true
		errorClass := classifyAssignmentsErr(err)
		// Observe so the receipt records the affected count; if Observe
		// returns false the receipt was already emitted this poll and we
		// must not duplicate it.
		if dedup.Observe("assignments", errorClass, "") {
			emitAssignmentsTopLevelReceipt(writer, matcher, kid, libVersion, err, errorClass, dedup)
		}
		return 0, errsLogged
	}

	// Emit one row-parse receipt per unique (parser, errorClass) in this
	// poll. Per the task spec, keep the classification simple: all rows
	// collapse to a single "row_parse" bucket.
	for _, pe := range parseErrs {
		if pe == nil {
			continue
		}
		const errorClass = "row_parse"
		if dedup.Observe("assignments", errorClass, "") {
			errsLogged = true
			emitAssignmentsRowParseReceipt(writer, matcher, kid, libVersion, pe, errorClass, dedup)
		} else {
			// Still counts as an error logged for schema-drift purposes.
			errsLogged = true
		}
	}

	matchKey := "schoology:" + kid.Name + ":" + assignmentSurface
	rule, ok := matcher.Match(matchKey)
	if !ok {
		slog.Warn("schoology assignments no rule match",
			"match_key", matchKey, "kid", kid.Name)
		return 0, errsLogged
	}

	for _, a := range assignments {
		if a == nil {
			continue
		}

		decision, err := ShouldStage(cp, assignmentSurface, kid.Name, a.ID)
		if err != nil {
			slog.Error("schoology assignments checkpoint read failed",
				"kid", kid.Name, "assignment_id", a.ID, "error", err)
			continue
		}
		if !decision.Accept() {
			continue
		}

		opts := buildAssignmentItemOptions(a, rule)
		body := []byte(buildAssignmentBody(a))

		item, err := writer.NewItem(opts)
		if err != nil {
			slog.Warn("schoology assignment new item failed",
				"kid", kid.Name, "assignment_id", a.ID, "error", err)
			continue
		}
		if err := item.WriteContent(body); err != nil {
			slog.Warn("schoology assignment write content failed",
				"kid", kid.Name, "assignment_id", a.ID, "error", err)
			continue
		}
		if err := item.Commit(); err != nil {
			slog.Warn("schoology assignment commit failed",
				"kid", kid.Name, "assignment_id", a.ID, "error", err)
			continue
		}
		if err := SaveLastSeenID(cp, assignmentSurface, kid.Name, a.ID); err != nil {
			// Item already committed; next poll will re-stage this
			// assignment if the checkpoint did not advance. Matches the
			// attachments.go duplicate_risk=true pattern.
			slog.Error("schoology assignment checkpoint save failed",
				"kid", kid.Name, "assignment_id", a.ID,
				"duplicate_risk", true, "error", err)
		}
		staged++
	}

	return staged, errsLogged
}

// buildAssignmentItemOptions composes ItemOptions for one assignment per
// spec 12 §7.1 (adapted to the real schoology-go Assignment fields:
// there is no teacher name or creation timestamp).
func buildAssignmentItemOptions(a *schoologylib.Assignment, rule connector.MatchResult) connector.ItemOptions {
	sender := a.CourseTitle
	if strings.TrimSpace(sender) == "" {
		// Staging validation rejects empty Sender; supply a non-PII
		// placeholder so the item still flows.
		sender = "schoology-course"
	}
	tags := map[string]string{}
	if a.CourseTitle != "" {
		tags["course"] = a.CourseTitle
	}
	if !a.DueAt.IsZero() {
		tags["due_date"] = a.DueAt.UTC().Format(time.RFC3339)
	}
	if a.Status != "" {
		tags["status"] = string(a.Status)
	}
	return connector.ItemOptions{
		Source:           "schoology",
		Sender:           sender,
		Subject:          a.Title,
		Timestamp:        time.Now().UTC(),
		DestinationAgent: rule.Destination,
		ContentType:      "text/plain",
		Tags:             tags,
		RuleTags:         rule.Tags,
		DataSubject:      rule.DataSubject,
		Audience:         rule.Audience,
	}
}

// buildAssignmentBody synthesizes a plaintext content.raw body for an
// assignment. The schoology-go library does not expose an assignment
// description, so we project the structured fields into a readable
// summary for the downstream scanner.
func buildAssignmentBody(a *schoologylib.Assignment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Assignment: %s\n", a.Title)
	if a.CourseTitle != "" {
		fmt.Fprintf(&b, "Course: %s\n", a.CourseTitle)
	}
	if !a.DueAt.IsZero() {
		fmt.Fprintf(&b, "Due: %s\n", a.DueAt.UTC().Format(time.RFC3339))
	}
	if a.Status != "" {
		fmt.Fprintf(&b, "Status: %s\n", a.Status)
	}
	if a.URL != "" {
		fmt.Fprintf(&b, "Link: %s\n", a.URL)
	}
	return b.String()
}

// classifyAssignmentsErr maps a schoology-go error to one of the short
// stable classification strings used as the parse-failure error_class
// tag. Mirrors classifyLibError in feed.go but is kept local per the
// task brief (no shared cross-processor helper yet).
func classifyAssignmentsErr(err error) string {
	switch {
	case err == nil:
		return "unknown"
	case schoologylib.IsAuthError(err):
		return "auth"
	case schoologylib.IsNotFoundError(err):
		return "not_found"
	case schoologylib.IsRateLimitError(err):
		return "rate_limit"
	case schoologylib.IsPermissionError(err):
		return "permission"
	case schoologylib.IsParseError(err):
		return "parse"
	}
	var libErr *schoologylib.Error
	if errors.As(err, &libErr) {
		return string(libErr.Code)
	}
	return "unknown"
}

// emitAssignmentsTopLevelReceipt stages a parse-failure receipt for a
// hard GetOverdueSubmissions error. Receipt routing uses match keys
// "schoology-parse-failure:assignments" with fallback to
// "schoology-parse-failure:*". If neither matches, the receipt is
// dropped silently with slog.Warn.
func emitAssignmentsTopLevelReceipt(
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	kid Kid,
	libVersion string,
	err error,
	errorClass string,
	dedup *ReceiptDedup,
) {
	in := ReceiptInputs{
		Parser:            "assignments",
		ErrorClass:        errorClass,
		ErrorMsg:          err.Error(),
		Trace:             "GetOverdueSubmissions",
		SourceURL:         fmt.Sprintf("schoology://overdue-submissions/%d", kid.SchoologyUID),
		TargetKid:         kid.Name,
		TargetContentType: "assignment",
		LibVersion:        libVersion,
		AffectedCount:     dedup.AffectedCount("assignments", errorClass),
		AffectedItemIDs:   dedup.AffectedItemIDs("assignments", errorClass),
	}
	if in.AffectedCount < 1 {
		in.AffectedCount = 1
	}
	emitAssignmentsReceipt(writer, matcher, in, []byte(err.Error()))
}

// emitAssignmentsRowParseReceipt stages a parse-failure receipt for a
// single bucket of row-parse failures.
func emitAssignmentsRowParseReceipt(
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	kid Kid,
	libVersion string,
	first *schoologylib.Error,
	errorClass string,
	dedup *ReceiptDedup,
) {
	affected := dedup.AffectedCount("assignments", errorClass)
	if affected < 1 {
		affected = 1
	}
	in := ReceiptInputs{
		Parser:            "assignments",
		ErrorClass:        errorClass,
		ErrorMsg:          first.Error(),
		Trace:             first.Op,
		SourceURL:         fmt.Sprintf("schoology://overdue-submissions/%d", kid.SchoologyUID),
		TargetKid:         kid.Name,
		TargetContentType: "assignment",
		LibVersion:        libVersion,
		AffectedCount:     affected,
		AffectedItemIDs:   dedup.AffectedItemIDs("assignments", errorClass),
	}
	emitAssignmentsReceipt(writer, matcher, in, []byte(first.Error()))
}

// emitAssignmentsReceipt is the local receipt staging path for this
// processor. Routes via the schoology-parse-failure rules and drops
// silently if no rule matches. The task brief asks us to keep this
// helper local rather than dedupe with feed.go's emitReceipt; a future
// cleanup commit can consolidate the two implementations.
func emitAssignmentsReceipt(
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	in ReceiptInputs,
	body []byte,
) {
	primary := "schoology-parse-failure:" + in.Parser
	rule, ok := matcher.Match(primary)
	if !ok {
		rule, ok = matcher.Match("schoology-parse-failure:*")
	}
	if !ok {
		slog.Warn("schoology assignments parse-failure receipt has no matching rule",
			"parser", in.Parser, "error_class", in.ErrorClass)
		return
	}
	opts := BuildReceiptOptions(in)
	opts.DestinationAgent = rule.Destination
	if len(rule.Audience) > 0 {
		opts.Audience = rule.Audience
	}
	if rule.DataSubject != "" {
		opts.DataSubject = rule.DataSubject
	}
	item, err := writer.NewItem(opts)
	if err != nil {
		slog.Warn("schoology assignments parse-failure receipt new item failed",
			"parser", in.Parser, "error", err)
		return
	}
	if err := item.WriteContent(body); err != nil {
		slog.Warn("schoology assignments parse-failure receipt write content failed",
			"parser", in.Parser, "error", err)
		return
	}
	if err := item.Commit(); err != nil {
		slog.Warn("schoology assignments parse-failure receipt commit failed",
			"parser", in.Parser, "error", err)
		return
	}
}
