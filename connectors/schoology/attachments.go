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

// ProcessAttachments downloads each attachment, enforces the size cap,
// dedupes via the (surface, scope) checkpoint, runs the routing rule,
// and stages each one as its own item. Returns the count successfully
// staged.
//
// On a checkpoint read error the attachment is skipped (logged); the
// poll continues. On a download error the attachment is skipped.
// Attachments exceeding maxSizeMB are skipped with a warn log.
func ProcessAttachments(
	ctx context.Context,
	client SchoologyClient,
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	cp connector.Checkpoint,
	atts []Attachment,
	parentID int64,
	parentType string,
	matchKey string,
	checkpointSurface string,
	checkpointScope string,
	maxSizeMB int,
) int {
	count := 0
	maxBytes := int64(maxSizeMB) * 1024 * 1024
	for _, a := range atts {
		d, err := ShouldStage(cp, checkpointSurface, checkpointScope, a.ID)
		if err != nil {
			slog.Error("schoology checkpoint read failed",
				"surface", checkpointSurface, "scope", checkpointScope,
				"attachment_id", a.ID, "error", err)
			continue
		}
		if !d.Accept() {
			continue
		}
		data, downloadErr := downloadCapped(ctx, client, a, maxBytes)
		if downloadErr != nil {
			slog.Warn("schoology attachment download failed",
				"attachment_id", a.ID, "filename", a.Filename, "error", downloadErr)
			continue
		}
		if int64(len(data)) > maxBytes {
			slog.Warn("schoology attachment too large",
				"attachment_id", a.ID, "filename", a.Filename,
				"size_bytes", len(data), "max_bytes", maxBytes)
			continue
		}
		result, ok := matcher.Match(matchKey)
		if !ok {
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
			continue
		}
		if err := SaveLastSeenID(cp, checkpointSurface, checkpointScope, a.ID); err != nil {
			slog.Warn("schoology attachment checkpoint save failed",
				"attachment_id", a.ID, "error", err)
		}
		count++
	}
	return count
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
