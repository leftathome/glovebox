package ingest

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// buildMultipartWithSidecars builds a metadata + content body plus one
// "sidecar" part per entry in sidecars (filename -> bytes).
func buildMultipartWithSidecars(metadataJSON string, content []byte, sidecars map[string][]byte) (*bytes.Buffer, string) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	metaPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="metadata"; filename="metadata.json"`},
		"Content-Type":        {"application/json"},
	})
	metaPart.Write([]byte(metadataJSON))

	contentPart, _ := writer.CreatePart(map[string][]string{
		"Content-Disposition": {`form-data; name="content"; filename="content.raw"`},
		"Content-Type":        {"application/octet-stream"},
	})
	contentPart.Write(content)

	for name, data := range sidecars {
		p, _ := writer.CreatePart(map[string][]string{
			"Content-Disposition": {`form-data; name="sidecar"; filename="` + name + `"`},
			"Content-Type":        {"application/octet-stream"},
		})
		p.Write(data)
	}

	writer.Close()
	return body, writer.FormDataContentType()
}

func postBody(t *testing.T, url string, body *bytes.Buffer, contentType string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, contentType, body)
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	return resp
}

// TestSidecarPartsPersisted verifies the ingest handler accepts "sidecar"
// parts and writes them into the staged item dir alongside content.raw and
// metadata.json, so HTTP-backend enrichment artifacts (glovebox-afq4.12)
// land on disk for downstream consumers.
func TestSidecarPartsPersisted(t *testing.T) {
	h, stagingDir := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	sidecars := map[string][]byte{
		"content.extracted.md":              []byte("EXTRACTED TEXT"),
		"content.attachment-1.extracted.md": []byte("ATTACHMENT TEXT"),
	}
	body, ct := buildMultipartWithSidecars(validMetadataJSON("home-agent"), []byte("raw body"), sidecars)
	resp := postBody(t, ts.URL+"/v1/ingest", body, ct)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 202, got %d: %s", resp.StatusCode, string(b))
	}

	var result map[string]string
	bdata, _ := io.ReadAll(resp.Body)
	_ = bdata
	// Find the staged item dir (single item under stagingDir).
	entries, _ := os.ReadDir(stagingDir)
	var itemDir string
	for _, e := range entries {
		if e.IsDir() && e.Name() != ".ingest-tmp" {
			itemDir = filepath.Join(stagingDir, e.Name())
		}
	}
	if itemDir == "" {
		t.Fatalf("no staged item dir found (result=%v)", result)
	}

	for name, want := range sidecars {
		got, err := os.ReadFile(filepath.Join(itemDir, name))
		if err != nil {
			t.Errorf("sidecar %s not persisted: %v", name, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("sidecar %s = %q, want %q", name, got, want)
		}
	}
	// content.raw + metadata.json still present.
	for _, f := range []string{"content.raw", "metadata.json"} {
		if _, err := os.Stat(filepath.Join(itemDir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
}

// TestSidecarPathTraversalRejected verifies a sidecar filename that tries
// to escape the item dir is rejected with 400, not written outside the
// staging tree.
func TestSidecarPathTraversalRejected(t *testing.T) {
	h, stagingDir := newTestHandler(t, defaultIngestConfig(), []string{"home-agent"})
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	for _, bad := range []string{"../escape.md", "sub/dir.md", "..", ""} {
		body, ct := buildMultipartWithSidecars(
			validMetadataJSON("home-agent"), []byte("raw"),
			map[string][]byte{bad: []byte("x")},
		)
		resp := postBody(t, ts.URL+"/v1/ingest", body, ct)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("sidecar filename %q: expected 400, got %d", bad, resp.StatusCode)
		}
	}

	// Nothing should have been written outside; the escape target must not exist.
	if _, err := os.Stat(filepath.Join(filepath.Dir(stagingDir), "escape.md")); err == nil {
		t.Error("path-traversal sidecar escaped the staging dir")
	}
}
