package schoology

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/attribute"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/content"
	schoologylib "github.com/leftathome/schoology-go"
)

// feedSurface is the per-kid checkpoint surface for parent posts.
const feedSurface = "feed"

// feedAttachmentSurface is the per-kid checkpoint surface used by the
// attachments handler when called from the feed processor.
const feedAttachmentSurface = "feed_attachment"

// ProcessFeed polls one kid's parent home feed, stages every new post,
// and downloads any attachments through ProcessAttachments.
//
// Returns:
//   - staged: number of parent posts staged in this call.
//   - attachmentsStaged: total attachments staged across all posts.
//   - errsLogged: true if any parse-failure receipt was emitted (so a
//     SchemaDriftCounter caller can detect "errors but no items").
//
// Per-post failures (non-numeric EdgeID, checkpoint read failure, stage
// failure) are logged and skipped; the poll continues with the next
// post. Top-level GetFeed errors emit a single parse-failure receipt
// and short-circuit with staged=0.
func ProcessFeed(
	ctx context.Context,
	client SchoologyClient,
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	cp connector.Checkpoint,
	tel *Telemetry,
	dedup *ReceiptDedup,
	kid Kid,
	maxAttachmentMB int,
	libVersion string,
) (staged int, attachmentsStaged int, errsLogged bool) {
	posts, parseErrs, err := getFeedTraced(ctx, client, tel, kid)
	if err != nil {
		errsLogged = true
		errorClass := classifyLibError(err)
		// schoology_parse_failures_total fires on EVERY observation per
		// spec §13.1 ("parse failures"); the receipt counter fires only
		// on the first observation per poll because it tracks emitted
		// receipts, not failures.
		tel.RecordParseFailure("feed", errorClass, kid.Name, "feed")
		// Observe so the receipt records the affected count; if Observe
		// returns false the receipt was already emitted this poll and we
		// must not duplicate it. Mirrors the assignments processor so
		// processors that share a dedup tracker behave consistently.
		receiptEmitted := dedup.Observe("feed", errorClass, "")
		if receiptEmitted {
			emitFeedTopLevelReceipt(writer, matcher, kid, libVersion, err, errorClass)
			tel.RecordParseFailureReceipt("feed", errorClass)
		}
		slog.Warn("schoology parse failure",
			"parser", "feed",
			"error_class", errorClass,
			"kid", kid.Name,
			"receipt_emitted", receiptEmitted,
			"affected_count", dedup.AffectedCount("feed", errorClass),
		)
		return 0, 0, errsLogged
	}

	// Emit one row-parse receipt per unique (parser, errorClass) in this
	// poll. All row-parse failures collapse to a single "row_parse"
	// bucket — matches the assignments + messages processors so an
	// operator dashboarding on error_class doesn't see arbitrary
	// per-surface taxonomies.
	for _, pe := range parseErrs {
		if pe == nil {
			continue
		}
		const errorClass = "row_parse"
		// Per-occurrence failure counter — fires for every row parse error.
		tel.RecordParseFailure("feed", errorClass, kid.Name, "feed")
		receiptEmitted := dedup.Observe("feed", errorClass, "")
		errsLogged = true
		if receiptEmitted {
			emitFeedRowParseReceipt(writer, matcher, kid, libVersion, pe, errorClass, dedup)
			tel.RecordParseFailureReceipt("feed", errorClass)
		}
		slog.Warn("schoology parse failure",
			"parser", "feed",
			"error_class", errorClass,
			"kid", kid.Name,
			"receipt_emitted", receiptEmitted,
			"affected_count", dedup.AffectedCount("feed", errorClass),
		)
	}

	matchKey := "schoology:" + kid.Name + ":" + feedSurface
	rule, ok := matcher.Match(matchKey)
	if !ok {
		slog.Warn("schoology feed no rule match",
			"match_key", matchKey, "kid", kid.Name)
		return 0, 0, errsLogged
	}

	attachMatchKey := "schoology:" + kid.Name + ":attachment"

	for _, p := range posts {
		if p == nil {
			continue
		}
		edgeID, err := ParseID(p.EdgeID)
		if err != nil {
			slog.Warn("schoology feed non-numeric edge id",
				"kid", kid.Name, "edge_id", p.EdgeID, "error", err)
			continue
		}

		decision, err := ShouldStage(cp, feedSurface, kid.Name, edgeID)
		if err != nil {
			slog.Error("schoology feed checkpoint read failed",
				"kid", kid.Name, "edge_id", edgeID, "error", err)
			continue
		}
		if !decision.Accept() {
			continue
		}

		opts := buildFeedItemOptions(p, rule)
		body := content.HTMLToText([]byte(p.Body))

		if err := stageFeedItem(ctx, tel, writer, opts, body, edgeID); err != nil {
			slog.Warn("schoology feed stage failed",
				"kid", kid.Name, "edge_id", edgeID, "error", err)
			continue
		}
		if err := SaveLastSeenID(cp, feedSurface, kid.Name, edgeID); err != nil {
			// Item already committed; next poll will re-stage this post.
			slog.Error("schoology feed checkpoint save failed",
				"kid", kid.Name, "edge_id", edgeID,
				"duplicate_risk", true, "error", err)
		}
		staged++

		// Process attachments. Spec 12 §7.4 step 2 says skipped
		// attachments should add an `attachment_skipped` tag to the
		// parent item. Because the parent item has already committed at
		// this point, we can't amend it. Deferred to the umbrella
		// connector (glovebox-k5mr) which can re-shape the pipeline if
		// needed; for v1, skipped attachments surface only via logs
		// and the connector_items_dropped_total metric.
		if len(p.Attachments) == 0 {
			continue
		}
		atts := convertLibAttachments(p, opts.Subject)
		staged1, skipped := ProcessAttachments(
			ctx, client, writer, matcher, cp, tel,
			atts, edgeID, feedSurface,
			attachMatchKey, feedAttachmentSurface, kid.Name,
			maxAttachmentMB,
		)
		attachmentsStaged += staged1
		for _, s := range skipped {
			slog.Info("schoology feed attachment skipped",
				"kid", kid.Name, "edge_id", edgeID,
				"attachment_id", s.ID, "filename", s.Filename,
				"reason", s.Reason)
		}
	}

	return staged, attachmentsStaged, errsLogged
}

// getFeedTraced wraps client.GetFeed in the schoology.lib.GetFeed span
// (spec §13.2). Failures still propagate; the span exists for both
// success and failure cases so an operator can correlate latency with
// outcomes.
func getFeedTraced(ctx context.Context, client SchoologyClient, tel *Telemetry, kid Kid) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
	ctx, span := tel.StartSpan(ctx, "schoology.lib.GetFeed",
		attribute.Int64("uid", kid.SchoologyUID),
	)
	defer span.End()
	return client.GetFeed(ctx, kid.SchoologyUID)
}

// stageFeedItem writes the staging item under a schoology.staging.commit
// span. parse_status is "normal" for happy-path posts; the feed
// processor does not currently produce degraded items at this layer.
func stageFeedItem(ctx context.Context, tel *Telemetry, writer *connector.StagingWriter, opts connector.ItemOptions, body []byte, edgeID int64) error {
	_, span := tel.StartSpan(ctx, "schoology.staging.commit",
		attribute.Int64("item_id", edgeID),
		attribute.String("destination", opts.DestinationAgent),
		attribute.String("data_subject", opts.DataSubject),
		attribute.String("parse_status", "normal"),
	)
	defer span.End()

	item, err := writer.NewItem(opts)
	if err != nil {
		return err
	}
	if err := item.WriteContent(body); err != nil {
		return err
	}
	return item.Commit()
}

// buildFeedItemOptions composes the ItemOptions for a parent post per
// spec 12 §7.2.
func buildFeedItemOptions(p *schoologylib.Post, rule connector.MatchResult) connector.ItemOptions {
	sender := p.AuthorName
	if sender == "" {
		sender = "schoology-author"
	}
	subject := deriveFeedSubject(p)
	tags := map[string]string{
		// post_type is a static placeholder until the schoology-go library
		// distinguishes faculty updates from other post types. Spec 12
		// §7.2 reserves this tag for that future distinction.
		"post_type": "post",
	}
	if p.PostedTo != "" {
		tags["course"] = p.PostedTo
	}
	return connector.ItemOptions{
		Source:           "schoology",
		Sender:           sender,
		Subject:          subject,
		Timestamp:        p.Timestamp,
		DestinationAgent: rule.Destination,
		ContentType:      "text/plain",
		Tags:             tags,
		RuleTags:         rule.Tags,
		DataSubject:      rule.DataSubject,
		Audience:         rule.Audience,
	}
}

// deriveFeedSubject builds a non-empty Subject from Post.Body or falls
// back to the author. The library exposes no separate title.
func deriveFeedSubject(p *schoologylib.Post) string {
	body := strings.TrimSpace(p.Body)
	if body != "" {
		first := strings.SplitN(body, "\n", 2)[0]
		first = strings.TrimSpace(first)
		if len(first) > 80 {
			first = first[:77] + "..."
		}
		if first != "" {
			return first
		}
	}
	if p.AuthorName != "" {
		return "Post from " + p.AuthorName
	}
	return "Schoology post"
}

// convertLibAttachments maps the schoology-go []*Attachment slice on a
// post to the connector's local []Attachment, filling in the parent
// metadata fields required by ProcessAttachments.
func convertLibAttachments(p *schoologylib.Post, parentSubject string) []Attachment {
	if len(p.Attachments) == 0 {
		return nil
	}
	sender := p.AuthorName
	if sender == "" {
		sender = "schoology-author"
	}
	out := make([]Attachment, 0, len(p.Attachments))
	for _, a := range p.Attachments {
		if a == nil {
			continue
		}
		out = append(out, Attachment{
			ID:              a.ID,
			URL:             a.URL,
			Filename:        a.Filename,
			MimeType:        a.MimeType,
			ParentSender:    sender,
			ParentSubject:   parentSubject,
			ParentTimestamp: p.Timestamp,
		})
	}
	return out
}

// classifyLibError maps a schoology-go error to one of the short stable
// classification strings used as the parse-failure error_class tag.
func classifyLibError(err error) string {
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
	// Sniff *Error.Code without unwrapping if errors.As fails to find
	// one (avoids relying on Op == "GetFeed" in classifier).
	var libErr *schoologylib.Error
	if errors.As(err, &libErr) {
		return string(libErr.Code)
	}
	return "unknown"
}

// emitFeedTopLevelReceipt stages a parse-failure receipt for a hard
// GetFeed error. Receipt routing uses match keys
// "schoology-parse-failure:feed" with fallback to
// "schoology-parse-failure:*". If neither matches, the receipt is
// dropped silently with slog.Warn.
func emitFeedTopLevelReceipt(
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	kid Kid,
	libVersion string,
	err error,
	errorClass string,
) {
	in := ReceiptInputs{
		Parser:            "feed",
		ErrorClass:        errorClass,
		ErrorMsg:          err.Error(),
		Trace:             "GetFeed",
		TargetKid:         kid.Name,
		TargetContentType: "feed",
		LibVersion:        libVersion,
		AffectedCount:     1,
	}
	emitReceipt(writer, matcher, in, []byte(err.Error()))
}

// emitFeedRowParseReceipt stages a parse-failure receipt for a single
// (parser, errorClass) bucket of row-parse failures.
func emitFeedRowParseReceipt(
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	kid Kid,
	libVersion string,
	first *schoologylib.Error,
	errorClass string,
	dedup *ReceiptDedup,
) {
	affected := dedup.AffectedCount("feed", errorClass)
	if affected < 1 {
		affected = 1
	}
	in := ReceiptInputs{
		Parser:            "feed",
		ErrorClass:        errorClass,
		ErrorMsg:          first.Error(),
		Trace:             first.Op,
		TargetKid:         kid.Name,
		TargetContentType: "feed",
		LibVersion:        libVersion,
		AffectedCount:     affected,
		AffectedItemIDs:   dedup.AffectedItemIDs("feed", errorClass),
	}
	emitReceipt(writer, matcher, in, []byte(first.Error()))
}

// emitReceipt is the shared receipt staging path. Routes via the
// schoology-parse-failure rules and drops silently if no rule matches.
func emitReceipt(
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
		slog.Warn("schoology parse-failure receipt has no matching rule",
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
		slog.Warn("schoology parse-failure receipt new item failed",
			"parser", in.Parser, "error", err)
		return
	}
	if err := item.WriteContent(body); err != nil {
		slog.Warn("schoology parse-failure receipt write content failed",
			"parser", in.Parser, "error", err)
		return
	}
	if err := item.Commit(); err != nil {
		slog.Warn("schoology parse-failure receipt commit failed",
			"parser", in.Parser, "error", err)
		return
	}
}

