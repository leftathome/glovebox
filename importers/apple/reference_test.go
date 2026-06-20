// reference_test.go -- unit tests for the apple-import-reference.json parser
// (glovebox-yea7). The fixture mirrors the recognizer handoff contract
// (docs/handoffs/recognizer-apple-export-delivery.md SS5): a JSON object keyed
// by recognizer media_type, each value a per-bucket interpretation map.
package main

import (
	"testing"
)

// referenceFixture is a representative apple-import-reference.json body with two
// buckets (media-services + other-data), matching the handoff contract shape.
const referenceFixture = `{
  "archive/apple/media-services": {
    "data_files": [
      "Apple Media Services Information/iTunes and App-Book Re-download and Update History.csv",
      "Apple Media Services Information/Apple_Media_Services.zip"
    ],
    "guide_refs": [
      "Store Transaction Purchase and Free Apps Download History - Guide.pdf",
      "iTunes and App Store Hidden Purchases - Guide.pdf"
    ],
    "notes": "Apple_Media_Services.zip is a nested archive (100+ CSVs) covering all iTunes/App Store purchases, Apple Music/TV/Books play activity, subscriptions, payment history. Game Center arrives in a separate Part 2 of 2 zip."
  },
  "archive/apple/other-data": {
    "data_files": [
      "Other Data/iCloudUsageData.csv",
      "Other Data/Devices Registered.csv"
    ],
    "guide_refs": [
      "iCloud Usage Data - Guide.pdf"
    ],
    "notes": "Find My, iCloud usage, devices, Maps/Mail/Safari/Health usage."
  }
}`

func TestParseImportReferenceMediaServices(t *testing.T) {
	ref, err := ParseImportReference([]byte(referenceFixture))
	if err != nil {
		t.Fatalf("ParseImportReference returned error: %v", err)
	}

	if len(ref) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(ref))
	}

	ms, ok := ref["archive/apple/media-services"]
	if !ok {
		t.Fatalf("media-services bucket missing from parse result")
	}

	wantData := []string{
		"Apple Media Services Information/iTunes and App-Book Re-download and Update History.csv",
		"Apple Media Services Information/Apple_Media_Services.zip",
	}
	if got := ms.DataFiles; !equalStrings(got, wantData) {
		t.Errorf("DataFiles = %v, want %v", got, wantData)
	}

	wantGuides := []string{
		"Store Transaction Purchase and Free Apps Download History - Guide.pdf",
		"iTunes and App Store Hidden Purchases - Guide.pdf",
	}
	if got := ms.GuideRefs; !equalStrings(got, wantGuides) {
		t.Errorf("GuideRefs = %v, want %v", got, wantGuides)
	}

	if ms.Notes == "" {
		t.Errorf("Notes unexpectedly empty for media-services")
	}
}

func TestImportReferenceBucketLookup(t *testing.T) {
	ref, err := ParseImportReference([]byte(referenceFixture))
	if err != nil {
		t.Fatalf("ParseImportReference returned error: %v", err)
	}

	b, ok := ref.Bucket("archive/apple/other-data")
	if !ok {
		t.Fatalf("Bucket(other-data) = not found, want found")
	}
	if len(b.DataFiles) != 2 {
		t.Errorf("other-data DataFiles len = %d, want 2", len(b.DataFiles))
	}

	if _, ok := ref.Bucket("archive/apple/does-not-exist"); ok {
		t.Errorf("Bucket(does-not-exist) = found, want not found")
	}
}

func TestParseImportReferenceErrors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"empty", []byte("")},
		{"nilInput", nil},
		{"whitespaceOnly", []byte("   \n\t ")},
		{"malformedJSON", []byte("{ not json")},
		{"wrongType", []byte("[1, 2, 3]")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseImportReference(tc.data); err == nil {
				t.Errorf("ParseImportReference(%q) = nil error, want error", tc.data)
			}
		})
	}
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
