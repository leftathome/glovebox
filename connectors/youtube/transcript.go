package main

import "context"

// TranscriptProvider fetches a video's transcript (glovebox-iwqm).
//
// The YouTube Data API cannot supply one for videos the caller does not own:
// captions.download needs edit permission on the video and returns 403
// otherwise. Any working provider therefore lives outside the official API, and
// choosing one is an operator decision. This seam keeps that choice a provider
// swap rather than a redesign. The default is noTranscriptProvider.
//
// A provider must never return an error for "this video has no transcript";
// that is a transcriptResult with Status transcriptUnavailable and a Reason.
// Errors are for failures worth logging (network, tool crash).
type TranscriptProvider interface {
	Transcript(ctx context.Context, videoID string, langs []string) (transcriptResult, error)
}

// Transcript availability, rendered on every item so a missing transcript is
// legible downstream instead of silently absent.
const (
	transcriptAvailable   = "available"
	transcriptUnavailable = "unavailable"
	transcriptSkipped     = "skipped"
	transcriptError       = "error"
)

type transcriptResult struct {
	Status   string
	Language string // set when available
	Text     string // set when available
	Reason   string // why it is not available / skipped / errored
}

// marker is the one-line "transcript:" value rendered into the item.
func (r transcriptResult) marker() string {
	switch r.Status {
	case transcriptAvailable:
		if r.Language != "" {
			return transcriptAvailable + " (" + r.Language + ")"
		}
		return transcriptAvailable
	case "":
		return transcriptUnavailable
	}
	if r.Reason != "" {
		return r.Status + " (" + r.Reason + ")"
	}
	return r.Status
}

// noTranscriptProvider is the default: it reports every transcript as
// unavailable, with a reason that says why.
type noTranscriptProvider struct{}

func (noTranscriptProvider) Transcript(context.Context, string, []string) (transcriptResult, error) {
	return transcriptResult{Status: transcriptUnavailable, Reason: "no transcript provider configured"}, nil
}
