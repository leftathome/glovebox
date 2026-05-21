package schoology

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/leftathome/glovebox/connector"
)

// Attachment is the per-attachment shape the connector passes to
// ProcessAttachments. The MimeType comes from the parent resource
// (feed post or message) since DownloadAttachment returns raw body
// without a content-type header.
type Attachment struct {
	ID              int64
	URL             string
	Filename        string
	MimeType        string
	ParentSender    string
	ParentSubject   string
	ParentTimestamp time.Time
}

// Skip reasons reported in AttachmentSkip.Reason. Stable strings; used
// as metric labels and as the value of the spec §7.4 parent-item
// attachment_skipped tag.
const (
	SkipSizeExceeded         = "size_exceeded"
	SkipDownloadError        = "download_error"
	SkipStageFailed          = "stage_failed"
	SkipNoRuleMatch          = "no_rule_match"
	SkipCheckpointReadFailed = "checkpoint_read_failed"
)

// AttachmentSkip records why an attachment was not staged. Returned by
// ProcessAttachments so the caller (future feed/message processor) can
// emit the spec §7.4 attachment_skipped tag on the parent item.
type AttachmentSkip struct {
	ID       int64
	Filename string
	Reason   string
}

// ProcessAttachments downloads each attachment, enforces the size cap,
// dedupes via the (surface, scope) checkpoint, runs the routing rule,
// and stages each one as its own item.
//
// Returns the count successfully staged and a per-attachment record of
// skips. The caller should use the skips to tag the parent item with
// attachment_skipped per spec 12 §7.4 step 2, and/or to surface metrics.
//
// Per-attachment failures (checkpoint read, download, oversized, no
// matching rule, stage failure) are logged and skipped; the poll
// continues. tel is optional (nil-safe); when non-nil, each skip emits
// both the framework connector_items_dropped_total{reason} counter and
// the schoology-specific schoology_attachments_skipped_total{reason}
// counter (spec §13.1).
func ProcessAttachments(
	ctx context.Context,
	client SchoologyClient,
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	cp connector.Checkpoint,
	tel *Telemetry,
	atts []Attachment,
	parentID int64,
	parentType string,
	matchKey string,
	checkpointSurface string,
	checkpointScope string,
	maxSizeMB int,
) (staged int, skipped []AttachmentSkip) {
	maxBytes := int64(maxSizeMB) * 1024 * 1024
	for _, a := range atts {
		d, err := ShouldStage(cp, checkpointSurface, checkpointScope, a.ID)
		if err != nil {
			slog.Error("schoology checkpoint read failed",
				"surface", checkpointSurface, "scope", checkpointScope,
				"attachment_id", a.ID, "error", err)
			skipped = append(skipped, AttachmentSkip{ID: a.ID, Filename: a.Filename, Reason: SkipCheckpointReadFailed})
			tel.RecordItemDropped(SkipCheckpointReadFailed)
			tel.RecordAttachmentSkipped(SkipCheckpointReadFailed)
			continue
		}
		if !d.Accept() {
			// Skipped by checkpoint (duplicate or below-checkpoint). Not
			// a failure -- no entry in skipped[], no metric.
			continue
		}
		data, downloadErr := downloadAttachmentCapped(ctx, client, tel, a, maxBytes)
		if downloadErr != nil {
			slog.Warn("schoology attachment download failed",
				"attachment_id", a.ID, "filename", a.Filename, "error", downloadErr)
			skipped = append(skipped, AttachmentSkip{ID: a.ID, Filename: a.Filename, Reason: SkipDownloadError})
			tel.RecordItemDropped(SkipDownloadError)
			tel.RecordAttachmentSkipped(SkipDownloadError)
			continue
		}
		if int64(len(data)) > maxBytes {
			slog.Warn("schoology attachment too large",
				"attachment_id", a.ID, "filename", a.Filename,
				"size_bytes", len(data), "max_bytes", maxBytes)
			skipped = append(skipped, AttachmentSkip{ID: a.ID, Filename: a.Filename, Reason: SkipSizeExceeded})
			tel.RecordItemDropped(SkipSizeExceeded)
			tel.RecordAttachmentSkipped(SkipSizeExceeded)
			continue
		}
		result, ok := matcher.Match(matchKey)
		if !ok {
			slog.Debug("schoology attachment no rule match",
				"match_key", matchKey, "attachment_id", a.ID, "filename", a.Filename)
			skipped = append(skipped, AttachmentSkip{ID: a.ID, Filename: a.Filename, Reason: SkipNoRuleMatch})
			tel.RecordItemDropped(SkipNoRuleMatch)
			tel.RecordAttachmentSkipped(SkipNoRuleMatch)
			continue
		}
		opts := connector.ItemOptions{
			Source:           "schoology",
			Sender:           a.ParentSender,
			Subject:          fmt.Sprintf("%s -- %s", a.ParentSubject, a.Filename),
			Timestamp:        a.ParentTimestamp,
			DestinationAgent: result.Destination,
			ContentType:      a.MimeType,
			Tags: map[string]string{
				"parent_id":   strconv.FormatInt(parentID, 10),
				"parent_type": parentType,
				"filename":    a.Filename,
				"size_bytes":  strconv.Itoa(len(data)),
			},
			DataSubject: result.DataSubject,
			Audience:    result.Audience,
		}
		if err := stageAttachmentContent(ctx, tel, writer, opts, data, a); err != nil {
			slog.Warn("schoology attachment stage failed",
				"attachment_id", a.ID, "filename", a.Filename, "error", err)
			skipped = append(skipped, AttachmentSkip{ID: a.ID, Filename: a.Filename, Reason: SkipStageFailed})
			tel.RecordItemDropped(SkipStageFailed)
			tel.RecordAttachmentSkipped(SkipStageFailed)
			continue
		}
		if err := SaveLastSeenID(cp, checkpointSurface, checkpointScope, a.ID); err != nil {
			// The item already committed. Failing to advance the checkpoint
			// here means the next poll will re-stage the same attachment
			// (duplicate ingest into the downstream pipeline). Surface at
			// Error severity so the duplicate-risk is visible in logs.
			slog.Error("schoology attachment checkpoint save failed",
				"attachment_id", a.ID, "filename", a.Filename,
				"duplicate_risk", true, "error", err)
		}
		// Successfully staged: record byte volume per spec §13.1
		// schoology_attachments_downloaded_bytes_total{kid, content_type}.
		// We use checkpointScope as the kid label because that's the per-
		// kid scope used by the feed/message callers; for the v1 surfaces
		// it always matches the kid name.
		tel.RecordAttachmentBytes(checkpointScope, a.MimeType, int64(len(data)))
		staged++
	}
	return staged, skipped
}

// downloadAttachmentCapped reads at most maxBytes+1 bytes from the
// attachment so the caller can detect "exceeded" by comparing
// len(data) > maxBytes. The schoology.lib.DownloadAttachment span (spec
// §13.2) wraps this call.
func downloadAttachmentCapped(ctx context.Context, client SchoologyClient, tel *Telemetry, a Attachment, maxBytes int64) ([]byte, error) {
	ctx, span := tel.StartSpan(ctx, "schoology.lib.DownloadAttachment",
		attribute.Int64("attachment_id", a.ID),
	)
	defer span.End()

	rc, err := client.DownloadAttachment(ctx, a.URL)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err == nil {
		span.SetAttributes(attribute.Int64("bytes", int64(len(data))))
	}
	return data, err
}

// stageAttachmentContent writes the staging item under a
// schoology.staging.commit span (spec §13.2). The parse_status attr is
// "normal" because attachment items don't carry a degraded shape -- the
// raw bytes are the content, with no separate parser.
func stageAttachmentContent(ctx context.Context, tel *Telemetry, writer *connector.StagingWriter, opts connector.ItemOptions, data []byte, a Attachment) error {
	_, span := tel.StartSpan(ctx, "schoology.staging.commit",
		attribute.Int64("item_id", a.ID),
		attribute.String("destination", opts.DestinationAgent),
		attribute.String("data_subject", opts.DataSubject),
		attribute.String("parse_status", "normal"),
	)
	defer span.End()

	item, err := writer.NewItem(opts)
	if err != nil {
		return err
	}
	if err := item.WriteContent(data); err != nil {
		return err
	}
	return item.Commit()
}
