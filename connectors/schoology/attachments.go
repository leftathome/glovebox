package schoology

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

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
// continues. metrics is optional (nil-safe); when non-nil, each skip
// increments connector_items_dropped_total{reason}.
func ProcessAttachments(
	ctx context.Context,
	client SchoologyClient,
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	cp connector.Checkpoint,
	metrics *connector.Metrics,
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
			recordDrop(metrics, SkipCheckpointReadFailed)
			continue
		}
		if !d.Accept() {
			// Skipped by checkpoint (duplicate or below-checkpoint). Not
			// a failure -- no entry in skipped[], no metric.
			continue
		}
		data, downloadErr := downloadCapped(ctx, client, a, maxBytes)
		if downloadErr != nil {
			slog.Warn("schoology attachment download failed",
				"attachment_id", a.ID, "filename", a.Filename, "error", downloadErr)
			skipped = append(skipped, AttachmentSkip{ID: a.ID, Filename: a.Filename, Reason: SkipDownloadError})
			recordDrop(metrics, SkipDownloadError)
			continue
		}
		if int64(len(data)) > maxBytes {
			slog.Warn("schoology attachment too large",
				"attachment_id", a.ID, "filename", a.Filename,
				"size_bytes", len(data), "max_bytes", maxBytes)
			skipped = append(skipped, AttachmentSkip{ID: a.ID, Filename: a.Filename, Reason: SkipSizeExceeded})
			recordDrop(metrics, SkipSizeExceeded)
			continue
		}
		result, ok := matcher.Match(matchKey)
		if !ok {
			slog.Debug("schoology attachment no rule match",
				"match_key", matchKey, "attachment_id", a.ID, "filename", a.Filename)
			skipped = append(skipped, AttachmentSkip{ID: a.ID, Filename: a.Filename, Reason: SkipNoRuleMatch})
			recordDrop(metrics, SkipNoRuleMatch)
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
		if err := stageContent(writer, opts, data); err != nil {
			slog.Warn("schoology attachment stage failed",
				"attachment_id", a.ID, "filename", a.Filename, "error", err)
			skipped = append(skipped, AttachmentSkip{ID: a.ID, Filename: a.Filename, Reason: SkipStageFailed})
			recordDrop(metrics, SkipStageFailed)
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
		staged++
	}
	return staged, skipped
}

// downloadCapped reads at most maxBytes+1 bytes from the attachment so
// the caller can detect "exceeded" by comparing len(data) > maxBytes.
func downloadCapped(ctx context.Context, client SchoologyClient, a Attachment, maxBytes int64) ([]byte, error) {
	rc, err := client.DownloadAttachment(ctx, a.URL)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxBytes+1))
}

func stageContent(writer *connector.StagingWriter, opts connector.ItemOptions, data []byte) error {
	item, err := writer.NewItem(opts)
	if err != nil {
		return err
	}
	if err := item.WriteContent(data); err != nil {
		return err
	}
	return item.Commit()
}

// recordDrop increments connector_items_dropped_total{reason} when
// metrics is non-nil. Task 12 (telemetry) may extend this to also emit
// the schoology-specific attachments_skipped_total counter.
func recordDrop(metrics *connector.Metrics, reason string) {
	if metrics == nil {
		return
	}
	metrics.RecordItemDropped(reason)
}
