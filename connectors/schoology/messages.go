// Inbox messages processor for spec 12 §7.3.
//
// V1 SCOPE LIMITATION: schoology-go v0.1.0 exposes inbox listing only --
// see ../schoology-go/messages.go. The MessageThread type carries
// thread-level metadata (subject, sender, last activity, message count)
// but NOT message bodies or attachments. As a result this processor:
//
//   - Synthesises a small text body from the listing metadata so the
//     scanner has something to operate on. The synthesised body lists
//     sender, subject, last-activity timestamp, message count, unread
//     flag, and link.
//   - Does NOT call ProcessAttachments. There are no attachments to
//     enumerate from a MessageThread; surfacing per-message attachments
//     requires a library-side GetThread / GetMessageBody extension that
//     is out of scope for the v1 connector.
//
// When the library grows a body / attachment surface, the natural
// extension is to fetch each accepted thread's body, replace the
// synthesised body with the real content, and call ProcessAttachments
// against any embedded attachments. The checkpoint surface ("message",
// scope="") stays the same.
package schoology

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	schoologylib "github.com/leftathome/schoology-go"

	"github.com/leftathome/glovebox/connector"
)

// ProcessMessages reads the parent-level inbox via GetInbox, stages each
// new thread (one per ID > checkpoint), and emits parse-failure receipts
// for top-level errors and per-row parse failures.
//
// Returns (staged, errsLogged):
//   - staged: count of threads successfully written to staging.
//   - errsLogged: true when at least one parse-failure receipt was
//     emitted OR when a top-level library error was hit. The poll
//     orchestrator uses this to drive SchemaDriftCounter.
func ProcessMessages(
	ctx context.Context,
	client SchoologyClient,
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	cp connector.Checkpoint,
	dedup *ReceiptDedup,
	tel *Telemetry,
	libVersion string,
) (staged int, errsLogged bool) {
	threads, parseErrs, err := getInboxTraced(ctx, client, tel)
	if err != nil {
		errsLogged = true
		errorClass := classifyMessagesError(err)
		// Per-occurrence parse-failure counter (spec §13.1). target_kid
		// is empty because the inbox is parent-level, not per-kid.
		tel.RecordParseFailure("messages", errorClass, "", "message")
		// Observe before emit so AffectedCount reflects reality; mirrors
		// the assignments + feed processors so a shared dedup tracker
		// behaves consistently across surfaces.
		receiptEmitted := dedup.Observe("messages", errorClass, "")
		if receiptEmitted {
			emitMessagesReceipt(writer, matcher, dedup, libVersion, "", errorClass, err.Error())
			tel.RecordParseFailureReceipt("messages", errorClass)
		}
		slog.Warn("schoology parse failure",
			"parser", "messages",
			"error_class", errorClass,
			"kid", "",
			"receipt_emitted", receiptEmitted,
			"affected_count", dedup.AffectedCount("messages", errorClass),
		)
		return 0, errsLogged
	}

	for _, pe := range parseErrs {
		if pe == nil {
			continue
		}
		tel.RecordParseFailure("messages", "row_parse", "", "message")
		receiptEmitted := dedup.Observe("messages", "row_parse", "")
		errsLogged = true
		if receiptEmitted {
			emitMessagesReceipt(writer, matcher, dedup, libVersion, "", "row_parse", pe.Error())
			tel.RecordParseFailureReceipt("messages", "row_parse")
		}
		slog.Warn("schoology parse failure",
			"parser", "messages",
			"error_class", "row_parse",
			"kid", "",
			"receipt_emitted", receiptEmitted,
			"affected_count", dedup.AffectedCount("messages", "row_parse"),
		)
	}

	result, ok := matcher.Match("schoology:message")
	if !ok {
		slog.Warn("schoology messages no rule match",
			"match_key", "schoology:message",
			"thread_count", len(threads))
		return 0, errsLogged
	}

	for _, t := range threads {
		if t == nil {
			continue
		}
		if t.ID == 0 {
			slog.Warn("schoology message has no thread id; skipping",
				"subject", t.Subject, "sender", t.SenderName)
			continue
		}
		d, sErr := ShouldStage(cp, "message", "", t.ID)
		if sErr != nil {
			slog.Error("schoology message checkpoint read failed",
				"thread_id", t.ID, "error", sErr)
			continue
		}
		if !d.Accept() {
			continue
		}

		opts, body := buildMessageItem(t, result)
		if err := stageMessageItem(ctx, tel, writer, opts, body, t.ID); err != nil {
			slog.Warn("schoology message stage failed",
				"thread_id", t.ID, "error", err)
			continue
		}
		if cpErr := SaveLastSeenID(cp, "message", "", t.ID); cpErr != nil {
			slog.Error("schoology message checkpoint save failed",
				"thread_id", t.ID,
				"duplicate_risk", true,
				"error", cpErr)
		}
		staged++
	}

	return staged, errsLogged
}

// getInboxTraced wraps client.GetInbox in the schoology.lib.GetInbox
// span (spec §13.2). The inbox is parent-level, so the span carries no
// uid attribute.
func getInboxTraced(ctx context.Context, client SchoologyClient, tel *Telemetry) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
	ctx, span := tel.StartSpan(ctx, "schoology.lib.GetInbox")
	defer span.End()
	return client.GetInbox(ctx)
}

// stageMessageItem writes the staging item under a schoology.staging.commit
// span. parse_status is "normal" because the messages processor stages
// only fully-parsed threads; degraded receipts go through the receipt
// path with their own emission helpers.
func stageMessageItem(ctx context.Context, tel *Telemetry, writer *connector.StagingWriter, opts connector.ItemOptions, body []byte, threadID int64) error {
	_, span := tel.StartSpan(ctx, "schoology.staging.commit",
		attribute.Int64("item_id", threadID),
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

// buildMessageItem synthesises ItemOptions plus the text body for a
// MessageThread. See the top-of-file note on the v1 scope: the body is
// composed from inbox-listing metadata, not the real message text.
func buildMessageItem(t *schoologylib.MessageThread, result connector.MatchResult) (connector.ItemOptions, []byte) {
	sender := t.SenderName
	if sender == "" {
		sender = "schoology-inbox"
	}
	subject := t.Subject
	if subject == "" {
		subject = "(no subject)"
	}
	ts := t.LastActivity
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	var lastActivityStr string
	if t.LastActivity.IsZero() {
		lastActivityStr = "unknown"
	} else {
		lastActivityStr = t.LastActivity.UTC().Format(time.RFC3339)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\n", sender)
	fmt.Fprintf(&b, "Subject: %s\n", subject)
	fmt.Fprintf(&b, "Last activity: %s\n", lastActivityStr)
	fmt.Fprintf(&b, "Messages in thread: %d\n", t.MessageCount)
	fmt.Fprintf(&b, "Unread: %t\n", t.Unread)
	fmt.Fprintf(&b, "Link: %s\n", t.URL)

	tags := map[string]string{
		"thread_id": strconv.FormatInt(t.ID, 10),
		"unread":    strconv.FormatBool(t.Unread),
	}
	if t.MessageCount > 0 {
		tags["message_count"] = strconv.Itoa(t.MessageCount)
	}

	opts := connector.ItemOptions{
		Source:           "schoology",
		Sender:           sender,
		Subject:          subject,
		Timestamp:        ts,
		DestinationAgent: result.Destination,
		ContentType:      "text/plain",
		Tags:             tags,
		DataSubject:      result.DataSubject,
		Audience:         result.Audience,
	}
	return opts, []byte(b.String())
}

// classifyMessagesError maps a library error to a stable short class
// label used in receipt tags. Unknown errors map to "unknown".
func classifyMessagesError(err error) string {
	switch {
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
	default:
		return "unknown"
	}
}

// emitMessagesReceipt writes a single parse-failure receipt for the
// messages parser. Routing follows spec 12 §11.3: try the
// per-parser key first, fall back to the wildcard, and drop with a
// warning if neither matches.
func emitMessagesReceipt(
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	dedup *ReceiptDedup,
	libVersion string,
	itemID string,
	errorClass string,
	errorMsg string,
) {
	const parser = "messages"
	primaryKey := "schoology-parse-failure:" + parser
	result, ok := matcher.Match(primaryKey)
	if !ok {
		result, ok = matcher.Match("schoology-parse-failure:*")
	}
	if !ok {
		slog.Warn("schoology messages parse-failure receipt dropped: no rule match",
			"match_key", primaryKey,
			"fallback_key", "schoology-parse-failure:*",
			"error_class", errorClass)
		return
	}

	// AffectedCount comes straight from the dedup tracker. Top-level and
	// row-parse callers both Observe() before invoking this helper, so
	// the count is at least 1 by the time we get here. (See ProcessMessages
	// near the top of this file.)
	opts := BuildReceiptOptions(ReceiptInputs{
		Parser:            parser,
		ErrorClass:        errorClass,
		ErrorMsg:          errorMsg,
		Trace:             "GetInbox",
		SourceURL:         "",
		TargetKid:         "",
		TargetContentType: "message",
		LibVersion:        libVersion,
		AffectedCount:     dedup.AffectedCount(parser, errorClass),
		AffectedItemIDs:   dedup.AffectedItemIDs(parser, errorClass),
	})
	opts.DestinationAgent = result.Destination
	if len(result.Audience) > 0 {
		opts.Audience = result.Audience
	}

	item, err := writer.NewItem(opts)
	if err != nil {
		slog.Warn("schoology messages receipt new item failed", "error", err)
		return
	}
	if err := item.WriteContent([]byte(errorMsg)); err != nil {
		slog.Warn("schoology messages receipt write content failed", "error", err)
		return
	}
	if err := item.Commit(); err != nil {
		slog.Warn("schoology messages receipt commit failed", "error", err)
		return
	}
	_ = itemID // reserved for future per-item receipts
}
