package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseChapters(t *testing.T) {
	for _, tc := range []struct {
		name string
		desc string
		want []chapter
	}{
		{
			name: "leading timestamps",
			desc: "Great video.\n\n0:00 Intro\n1:30 - Setup\n12:05 The build\n1:02:03 Outro\n",
			want: []chapter{{0, "Intro"}, {90, "Setup"}, {725, "The build"}, {3723, "Outro"}},
		},
		{
			name: "bullets, parens and separators",
			desc: "- (00:00) Start\n* 02:10 | Middle\n• 04:20: End",
			want: []chapter{{0, "Start"}, {130, "Middle"}, {260, "End"}},
		},
		{
			name: "trailing timestamps",
			desc: "Intro - 0:00\nPart two 3:00\nWrap up - 9:59",
			want: []chapter{{0, "Intro"}, {180, "Part two"}, {599, "Wrap up"}},
		},
		{
			// YouTube's own rule: the first chapter must start at 0:00.
			name: "first not at zero is not chapters",
			desc: "0:10 a\n1:00 b\n2:00 c",
			want: nil,
		},
		{
			name: "fewer than three is not chapters",
			desc: "0:00 a\n1:00 b",
			want: nil,
		},
		{
			name: "not ascending is not chapters",
			desc: "0:00 a\n5:00 b\n2:00 c",
			want: nil,
		},
		{
			name: "no timestamps",
			desc: "Just a description with a time of day 10am.",
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseChapters(tc.desc)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseChapters = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFormatOffset(t *testing.T) {
	for in, want := range map[int]string{0: "0:00", 65: "1:05", 3723: "1:02:03"} {
		if got := formatOffset(in); got != want {
			t.Errorf("formatOffset(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseISODuration(t *testing.T) {
	for in, want := range map[string]int{"PT12M3S": 723, "PT1H": 3600, "PT45S": 45, "P1DT1S": 86401, "": 0, "junk": 0} {
		if got := parseISODuration(in); got != want {
			t.Errorf("parseISODuration(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestBestThumbnail(t *testing.T) {
	th := map[string]thumbnail{
		"default": {URL: "d"}, "high": {URL: "h"}, "maxres": {URL: "m"},
	}
	if got := bestThumbnail(th); got != "m" {
		t.Fatalf("bestThumbnail = %q, want maxres", got)
	}
	th["uhd"] = thumbnail{URL: "u"}
	if got := bestThumbnail(th); got != "u" {
		t.Fatalf("bestThumbnail = %q, want uhd", got)
	}
	if got := bestThumbnail(nil); got != "" {
		t.Fatalf("bestThumbnail(nil) = %q, want empty", got)
	}
}

func TestEngagementBuckets(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	ago := func(days int) time.Time { return now.Add(-time.Duration(days) * 24 * time.Hour) }

	// Nine peers, all 10 days old, views/day 100..900 and comments/day 1..9.
	var peers []peerStats
	for i := 1; i <= 9; i++ {
		peers = append(peers, peerStats{
			ID: string(rune('a' + i - 1)), Published: ago(10),
			Views: uint64(i * 1000), Comments: uint64(i * 10), HasComments: true,
		})
	}
	b := computeBuckets(peers, now)

	for id, want := range map[string]string{"a": "low", "b": "low", "c": "low", "d": "medium", "f": "medium", "g": "high", "i": "high"} {
		if got := b[id].Viewership; got != want {
			t.Errorf("viewership[%s] = %q, want %q", id, got, want)
		}
		if got := b[id].Comments; got != want {
			t.Errorf("comments[%s] = %q, want %q", id, got, want)
		}
	}

	// Too few peers to compare against: "unknown", never a guess.
	small := computeBuckets(peers[:3], now)
	if got := small["a"].Viewership; got != "unknown" {
		t.Errorf("small peer set viewership = %q, want unknown", got)
	}

	// Comments disabled: no comment bucket.
	peers[0].HasComments = false
	if got := computeBuckets(peers, now)["a"].Comments; got != "unknown" {
		t.Errorf("comments-disabled bucket = %q, want unknown", got)
	}
}

func TestRenderContentHasMachineFieldsAndNoRawCounts(t *testing.T) {
	v := renderInput{
		VideoID:       "vid123",
		Title:         "How I built it",
		ChannelID:     "UCabc",
		ChannelTitle:  "Jeff Geerling",
		ChannelHandle: "@JeffGeerling",
		PublishedAt:   "2026-11-01T12:00:00Z",
		DurationSec:   723,
		Thumbnail:     "https://i.ytimg.com/vi/vid123/maxresdefault.jpg",
		CategoryID:    "28",
		Tags:          []string{"pi", "homelab"},
		Topics:        []string{"https://en.wikipedia.org/wiki/Technology"},
		Language:      "en",
		Description:   "Desc line.\n0:00 Intro\n1:00 Mid\n2:00 End",
		Chapters:      []chapter{{0, "Intro"}, {60, "Mid"}, {120, "End"}},
		Buckets:       buckets{Viewership: "high", Comments: "medium"},
		Comments:      []comment{{Author: "@someone", Text: "Nice <b>build</b> &amp; video"}},
		Transcript:    transcriptResult{Status: transcriptUnavailable, Reason: "no transcript provider configured"},
	}
	got := renderContent(v)

	for _, want := range []string{
		"# How I built it",
		"- video_id: vid123",
		"- watch_url: https://www.youtube.com/watch?v=vid123",
		"- thumbnail_url: https://i.ytimg.com/vi/vid123/maxresdefault.jpg",
		"- channel: Jeff Geerling (@JeffGeerling, UCabc)",
		"- channel_url: https://www.youtube.com/channel/UCabc",
		"- published_at: 2026-11-01T12:00:00Z",
		"- duration: 12:03",
		"- tags: pi, homelab",
		"- topics: Technology",
		"- viewership: high",
		"- comment_activity: medium",
		"- transcript: unavailable (no transcript provider configured)",
		"## Chapters",
		"- 0:00 Intro (offset_seconds: 0)",
		"- 2:00 End (offset_seconds: 120)",
		"## Description",
		"Desc line.",
		"## Top comments",
		"- @someone: Nice build & video",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("content missing %q\n---\n%s", want, got)
		}
	}
}

func TestRenderContentIncludesTranscriptWhenAvailable(t *testing.T) {
	got := renderContent(renderInput{
		VideoID: "v", Title: "t",
		Transcript: transcriptResult{Status: transcriptAvailable, Language: "en", Text: "hello world"},
	})
	if !strings.Contains(got, "- transcript: available (en)") || !strings.Contains(got, "## Transcript\n\nhello world") {
		t.Fatalf("transcript not rendered:\n%s", got)
	}
}
