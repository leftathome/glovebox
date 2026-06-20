package main

import (
	"context"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
)

// TestImport_MediaServices_NormalizesPurchases drives the priority
// iTunes-purchase acceptance (openclaw-e6f): a finalized media-services
// delivery whose tree/ holds the nested Apple_Media_Services.zip (containing the
// real "Store Transaction Purchase and Free Apps History.csv") must yield a
// queryable, LLM-readable markdown item listing the purchased titles, stamped
// with the config data_subject and the apple_bucket tag.
func TestImport_MediaServices_NormalizesPurchases(t *testing.T) {
	// Real-style headers from the Apple export's Store Transaction file.
	csv := "Apple Account Number,Item Purchased Date,Content Type,Item Reference Number,Item Description,Invoice Item Total,Currency,Order Number\n" +
		"10655968,2026-05-20T17:56:28Z,Mac Apps,747648890,Telegram,,USD,R24YTVVSHL7TJ3\n" +
		"10655968,2026-05-16T02:58:13Z,iOS and tvOS Apps,300704847,Speedtest by Ookla,$0.00,USD,R24YSL5MFT0D6G\n" +
		"10655968,2026-04-01T10:00:00Z,Music,111,Some Song,$1.29,USD,R000\n"

	zipBytes := buildZip(t, map[string]string{
		"Apple_Media_Services/Stores Activity/Account and Transaction History/Store Transaction Purchase and Free Apps History.csv": csv,
	})

	archiveDir := writeArchive(t, appleReceipt(), map[string][]byte{
		"Apple_Media_Services.zip": zipBytes,
	})

	m, stagingDir := newTestImporter(t, []connector.Rule{
		{Match: "*", Destination: "media-agent"},
	})

	if err := m.Import(context.Background(), archiveDir, nil, nil, zeroDecision()); err != nil {
		t.Fatalf("Import: %v", err)
	}

	items := collectStaged(t, stagingDir)

	var md *stagedItem
	for i := range items {
		if items[i].meta.ContentType == "text/markdown" {
			md = &items[i]
			break
		}
	}
	if md == nil {
		t.Fatalf("no text/markdown purchase item staged; got %d items", len(items))
	}

	content := string(md.content)
	for _, title := range []string{"Telegram", "Speedtest by Ookla", "Some Song"} {
		if !strings.Contains(content, title) {
			t.Errorf("purchase markdown missing title %q; content:\n%s", title, content)
		}
	}
	if !strings.Contains(content, "Store Transaction Purchase and Free Apps History.csv") {
		t.Errorf("purchase markdown missing source filename; content:\n%s", content)
	}

	if md.meta.DataSubject != appleDataSubjectDefault {
		t.Errorf("DataSubject = %q, want %q", md.meta.DataSubject, appleDataSubjectDefault)
	}
	if md.meta.Tags["apple_bucket"] != "apple/media-services" {
		t.Errorf("tags[apple_bucket] = %q, want apple/media-services", md.meta.Tags["apple_bucket"])
	}
}
