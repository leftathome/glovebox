package main

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file turns one video's API data into the staged item body. The body is
// markdown because triage hands content.raw straight to caro and agents read it
// as text. Machine-readable fields come first as unambiguous "- key: value"
// lines so a later agent (or a refresh) can parse them back out.
//
// Raw view, like and comment COUNTS are never rendered. YouTube Developer
// Policy III.E.4 bars storing statistics from API-key reads for more than 30
// days, and the operator only needs a relative signal (glovebox-8wu1), so the
// counts are reduced to high/medium/low buckets at fetch time and discarded.

// chapter is one entry of a video's chapter list.
type chapter struct {
	OffsetSec int
	Title     string
}

// comment is one top-level comment, already reduced to display text.
type comment struct {
	Author string
	Text   string
}

// buckets are the relative engagement labels for one video.
type buckets struct {
	Viewership string // high | medium | low | unknown
	Comments   string // high | medium | low | unknown
}

// renderInput is everything renderContent needs for one video.
type renderInput struct {
	VideoID       string
	Title         string
	ChannelID     string
	ChannelTitle  string
	ChannelHandle string
	PublishedAt   string
	DurationSec   int
	Thumbnail     string
	CategoryID    string
	Tags          []string
	Topics        []string
	Language      string
	LiveContent   string // snippet.liveBroadcastContent: none | upcoming | live
	Description   string
	Chapters      []chapter
	Buckets       buckets
	BucketNote    string
	Comments      []comment
	Transcript    transcriptResult
}

func watchURL(videoID string) string {
	return "https://www.youtube.com/watch?v=" + url.QueryEscape(videoID)
}

// renderContent renders the staged markdown body for one video.
func renderContent(v renderInput) string {
	var b strings.Builder
	line := func(k, val string) {
		if val != "" {
			fmt.Fprintf(&b, "- %s: %s\n", k, oneLine(val))
		}
	}

	fmt.Fprintf(&b, "# %s\n\n", oneLine(v.Title))
	line("video_id", v.VideoID)
	line("watch_url", watchURL(v.VideoID))
	line("thumbnail_url", v.Thumbnail)
	if v.ChannelTitle != "" || v.ChannelID != "" {
		var ids []string
		for _, s := range []string{v.ChannelHandle, v.ChannelID} {
			if s != "" {
				ids = append(ids, s)
			}
		}
		line("channel", strings.TrimSpace(fmt.Sprintf("%s (%s)", v.ChannelTitle, strings.Join(ids, ", "))))
	}
	if v.ChannelID != "" {
		line("channel_url", "https://www.youtube.com/channel/"+url.PathEscape(v.ChannelID))
	}
	line("published_at", v.PublishedAt)
	if v.DurationSec > 0 {
		line("duration", formatOffset(v.DurationSec))
	}
	line("category_id", v.CategoryID)
	line("tags", strings.Join(v.Tags, ", "))
	line("topics", strings.Join(topicNames(v.Topics), ", "))
	line("default_audio_language", v.Language)
	if v.LiveContent != "" && v.LiveContent != "none" {
		line("live", v.LiveContent)
	}
	line("viewership", v.Buckets.Viewership)
	line("comment_activity", v.Buckets.Comments)
	line("engagement_note", v.BucketNote)
	line("transcript", v.Transcript.marker())

	if len(v.Chapters) > 0 {
		b.WriteString("\n## Chapters\n\n")
		for _, c := range v.Chapters {
			fmt.Fprintf(&b, "- %s %s (offset_seconds: %d)\n", formatOffset(c.OffsetSec), oneLine(c.Title), c.OffsetSec)
		}
	}

	if d := strings.TrimSpace(v.Description); d != "" {
		b.WriteString("\n## Description\n\n")
		b.WriteString(d)
		b.WriteString("\n")
	}

	if len(v.Comments) > 0 {
		b.WriteString("\n## Top comments\n\n")
		for _, c := range v.Comments {
			text := oneLine(stripHTML(c.Text))
			if c.Author != "" {
				fmt.Fprintf(&b, "- %s: %s\n", oneLine(c.Author), text)
			} else {
				fmt.Fprintf(&b, "- %s\n", text)
			}
		}
	}

	if v.Transcript.Status == transcriptAvailable && strings.TrimSpace(v.Transcript.Text) != "" {
		b.WriteString("\n## Transcript\n\n")
		b.WriteString(strings.TrimSpace(v.Transcript.Text))
		b.WriteString("\n")
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// stripHTML reduces commentThreads textDisplay (which carries <br>, <a>, <b>
// and entities) to plain text.
func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "<br>", " ")
	return html.UnescapeString(htmlTag.ReplaceAllString(s, ""))
}

// topicNames turns topicDetails.topicCategories Wikipedia URLs into names.
func topicNames(urls []string) []string {
	var out []string
	for _, u := range urls {
		name := u
		if i := strings.LastIndex(u, "/"); i >= 0 {
			name = u[i+1:]
		}
		if dec, err := url.PathUnescape(name); err == nil {
			name = dec
		}
		name = strings.ReplaceAll(name, "_", " ")
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// formatOffset renders seconds as m:ss or h:mm:ss, the way YouTube does.
func formatOffset(sec int) string {
	h, m, s := sec/3600, (sec%3600)/60, sec%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

var isoDuration = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// parseISODuration parses contentDetails.duration (ISO 8601, e.g. PT12M3S).
// Unparseable input yields 0, which renderContent omits.
func parseISODuration(s string) int {
	m := isoDuration.FindStringSubmatch(s)
	if m == nil || s == "P" {
		return 0
	}
	mult := []int{0, 86400, 3600, 60, 1}
	total := 0
	for i := 1; i <= 4; i++ {
		if m[i] != "" {
			n, _ := strconv.Atoi(m[i])
			total += n * mult[i]
		}
	}
	return total
}

// thumbnail is one entry of snippet.thumbnails.
type thumbnail struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// thumbnailPreference is largest first. uhd/qhd/fhd were added to the API on
// 2026-09-11 for some videos; maxres is the usual best.
var thumbnailPreference = []string{"uhd", "qhd", "fhd", "maxres", "standard", "high", "medium", "default"}

func bestThumbnail(th map[string]thumbnail) string {
	for _, k := range thumbnailPreference {
		if t, ok := th[k]; ok && t.URL != "" {
			return t.URL
		}
	}
	return ""
}

// Chapter parsing. The API has no chapters field: chapters are timestamp lines
// in the description, and YouTube only treats them as chapters when the first
// is 0:00, there are at least three, and they ascend. We apply the same rule,
// so we never invent chapters YouTube itself would not show.
var (
	tsPattern       = `\(?\[?((?:\d{1,2}:)?\d{1,2}:\d{2})\]?\)?`
	leadingChapter  = regexp.MustCompile(`^\s*(?:[-*\x{2022}]\s*)?` + tsPattern + `\s*[-\x{2013}\x{2014}:|]?\s*(.+?)\s*$`)
	trailingChapter = regexp.MustCompile(`^\s*(?:[-*\x{2022}]\s*)?(.+?)\s*[-\x{2013}\x{2014}:|]?\s*` + tsPattern + `\s*$`)
)

func parseTimestamp(ts string) (int, bool) {
	parts := strings.Split(ts, ":")
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		total = total*60 + n
	}
	return total, true
}

func parseChapters(desc string) []chapter {
	var out []chapter
	for _, ln := range strings.Split(desc, "\n") {
		var ts, title string
		if m := leadingChapter.FindStringSubmatch(ln); m != nil {
			ts, title = m[1], m[2]
		} else if m := trailingChapter.FindStringSubmatch(ln); m != nil {
			title, ts = m[1], m[2]
		} else {
			continue
		}
		sec, ok := parseTimestamp(ts)
		if !ok || strings.TrimSpace(title) == "" {
			continue
		}
		out = append(out, chapter{OffsetSec: sec, Title: strings.TrimSpace(title)})
	}
	if len(out) < 3 || out[0].OffsetSec != 0 {
		return nil
	}
	for i := 1; i < len(out); i++ {
		if out[i].OffsetSec <= out[i-1].OffsetSec {
			return nil
		}
	}
	return out
}

// peerStats is one video's statistics, used only to compute buckets and then
// discarded.
type peerStats struct {
	ID          string
	Published   time.Time
	Views       uint64
	Comments    uint64
	HasComments bool // false when statistics.commentCount is absent (comments off)
}

// minPeers is the smallest peer set we will rank against. Below it a bucket
// would be noise, so we say "unknown" instead of guessing.
const minPeers = 5

// computeBuckets ranks each video against the channel's other recent uploads
// by views per day and comments per day since publish, and labels the bottom
// third low, the top third high, the rest medium. Rates are per day so an old
// video's lifetime total does not swamp a new one's; age is floored at one day.
func computeBuckets(peers []peerStats, now time.Time) map[string]buckets {
	rate := func(n uint64, pub time.Time) float64 {
		days := now.Sub(pub).Hours() / 24
		if days < 1 {
			days = 1
		}
		return float64(n) / days
	}
	label := func(vals map[string]float64) map[string]string {
		out := map[string]string{}
		if len(vals) < minPeers {
			return out
		}
		ids := make([]string, 0, len(vals))
		for id := range vals {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			if vals[ids[i]] == vals[ids[j]] {
				return ids[i] < ids[j]
			}
			return vals[ids[i]] < vals[ids[j]]
		})
		n := len(ids)
		for r, id := range ids {
			switch {
			case r*3 < n:
				out[id] = "low"
			case r*3 >= 2*n:
				out[id] = "high"
			default:
				out[id] = "medium"
			}
		}
		return out
	}

	views, comments := map[string]float64{}, map[string]float64{}
	for _, p := range peers {
		views[p.ID] = rate(p.Views, p.Published)
		if p.HasComments {
			comments[p.ID] = rate(p.Comments, p.Published)
		}
	}
	vl, cl := label(views), label(comments)

	out := map[string]buckets{}
	for _, p := range peers {
		b := buckets{Viewership: "unknown", Comments: "unknown"}
		if s, ok := vl[p.ID]; ok {
			b.Viewership = s
		}
		if s, ok := cl[p.ID]; ok {
			b.Comments = s
		}
		out[p.ID] = b
	}
	return out
}
