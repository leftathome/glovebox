package connector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/leftathome/glovebox/connector/enrich"
)

// HTTPStagingBackend implements StagingBackend by POSTing items to the
// scanner's /v1/ingest endpoint as multipart/form-data requests.
type HTTPStagingBackend struct {
	ingestURL                string
	connectorName            string
	httpClient               *http.Client
	retryMax                 int
	retryBase                time.Duration
	configIdentity           *ConfigIdentity
	configDataSubjectDefault string
	configAudienceDefault    []string
	// tier is the connector's channel-tier declaration, stamped onto every
	// item this backend produces. Set once by NewFramework via SetTier.
	tier Tier
	// enrichRegistry is the enrichment registry run by commitHTTP. Nil
	// means "use enrich.Default". Tests inject a fresh registry to avoid
	// leaking state through enrich.Default. See glovebox-afq4.12.
	enrichRegistry *enrich.Registry
}

// Compile-time check: *HTTPStagingBackend satisfies StagingBackend.
var _ StagingBackend = (*HTTPStagingBackend)(nil)

// NewHTTPStagingBackend creates a new HTTP-based staging backend that POSTs
// items to ingestURL. If httpClient is nil, a default client with a 30s
// timeout is used.
func NewHTTPStagingBackend(ingestURL, connectorName string, httpClient *http.Client) *HTTPStagingBackend {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPStagingBackend{
		ingestURL:     ingestURL,
		connectorName: connectorName,
		httpClient:    httpClient,
		retryMax:      3,
		retryBase:     1 * time.Second,
	}
}

// WithRetry configures retry parameters for testing. It returns the backend
// for method chaining.
func (h *HTTPStagingBackend) WithRetry(max int, base time.Duration) *HTTPStagingBackend {
	h.retryMax = max
	h.retryBase = base
	return h
}

// SetConfigIdentity sets the config-level identity used as the base for
// identity merging at Commit() time.
func (h *HTTPStagingBackend) SetConfigIdentity(ci *ConfigIdentity) {
	h.configIdentity = ci
}

// SetConfigDataSubject sets the config-level data_subject default used as
// the final fallback in the merge chain (spec 11 §5).
func (h *HTTPStagingBackend) SetConfigDataSubject(s string) {
	h.configDataSubjectDefault = s
}

// SetConfigAudience sets the config-level audience default used as the
// final fallback in the merge chain (spec 11 §5). The slice is copied to
// prevent aliasing into the caller's storage.
func (h *HTTPStagingBackend) SetConfigAudience(a []string) {
	if len(a) == 0 {
		h.configAudienceDefault = nil
		return
	}
	h.configAudienceDefault = append([]string(nil), a...)
}

// SetTier sets the connector's channel-tier declaration (connector/tier.go).
// Called once by NewFramework from Options.Tier.
func (h *HTTPStagingBackend) SetTier(t Tier) {
	h.tier = t
}

// SetEnrichRegistry overrides the enrichment registry run by commitHTTP.
// When unset (or nil), commitHTTP uses the process-global enrich.Default
// registry. Mirrors StagingWriter.SetEnrichRegistry so HTTP-backend items
// reach enrichment parity with filesystem items (glovebox-afq4.12); tests
// inject a fresh registry here to avoid leaking state through enrich.Default.
func (h *HTTPStagingBackend) SetEnrichRegistry(r *enrich.Registry) {
	h.enrichRegistry = r
}

// NewItem creates a StagingItem whose Commit() POSTs to the ingest endpoint
// instead of writing to the filesystem.
func (h *HTTPStagingBackend) NewItem(opts ItemOptions) (*StagingItem, error) {
	si := &StagingItem{
		opts:                     opts,
		configIdentity:           h.configIdentity,
		configDataSubjectDefault: h.configDataSubjectDefault,
		configAudienceDefault:    h.configAudienceDefault,
		tier:                     h.tier,
		enrichRegistry:           h.enrichRegistry,
	}

	tmpDir, err := os.MkdirTemp("", "glovebox-http-*")
	if err != nil {
		return nil, fmt.Errorf("create http backend tmp dir: %w", err)
	}
	si.dir = tmpDir

	si.commitFunc = func() error {
		return h.commitHTTP(si, tmpDir)
	}

	return si, nil
}

// sidecarFile is one enrichment artifact (or error marker) produced
// connector-side and sent to the ingest server as a multipart "sidecar"
// part so HTTP-backend items match the filesystem layout (glovebox-afq4.12).
type sidecarFile struct {
	name string
	data []byte
}

// commitHTTP builds metadata, runs the enrichment pipeline connector-side,
// reads content + the produced sidecar artifacts from the staging item's
// temp directory, and POSTs the multipart request with retries.
func (h *HTTPStagingBackend) commitHTTP(si *StagingItem, tmpDir string) error {
	defer os.RemoveAll(tmpDir)

	meta, err := si.buildMetadata()
	if err != nil {
		return err
	}

	// Run enrichment connector-side (parity with the filesystem Commit):
	// this populates meta.Enrichments[] and writes sidecar/marker files
	// into si.dir. Per-enricher failures never abort the commit
	// (glovebox-afq4.12 / spec 14 §4.4, §4.7).
	si.runEnrichmentPipeline(&meta)

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	contentPath := filepath.Join(si.dir, "content.raw")
	contentData, err := os.ReadFile(contentPath)
	if err != nil {
		contentData = nil
	}

	sidecars, err := collectSidecars(si.dir)
	if err != nil {
		return fmt.Errorf("collect enrichment sidecars: %w", err)
	}

	return h.postWithRetry(metaJSON, contentData, sidecars)
}

// collectSidecars returns every regular file in dir except content.raw and
// metadata.json (which are sent as their own parts). This is the set of
// enrichment artifacts, error markers, and any attachment-* files the
// connector staged. Returned in lexicographic order for deterministic
// requests.
func collectSidecars(dir string) ([]sidecarFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []sidecarFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "content.raw" || name == "metadata.json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, sidecarFile{name: name, data: data})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// drainAndClose reads remaining bytes from the response body and closes it,
// allowing HTTP connection reuse.
func drainAndClose(body io.ReadCloser) {
	io.Copy(io.Discard, body)
	body.Close()
}

// postWithRetry sends the multipart request and retries on transient errors.
func (h *HTTPStagingBackend) postWithRetry(metaJSON, content []byte, sidecars []sidecarFile) error {
	var lastErr error

	for attempt := 0; attempt <= h.retryMax; attempt++ {
		if attempt > 0 {
			h.sleep(h.backoffDuration(attempt - 1))
		}

		body, contentType, err := h.buildMultipart(metaJSON, content, sidecars)
		if err != nil {
			return fmt.Errorf("build multipart body: %w", err)
		}

		req, err := http.NewRequest(http.MethodPost, h.ingestURL, body)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-Glovebox-Connector", h.connectorName)

		resp, err := h.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http request failed: %w", err)
			continue
		}
		drainAndClose(resp.Body)

		switch {
		case resp.StatusCode == http.StatusAccepted:
			return nil
		case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge:
			return PermanentError(fmt.Errorf("ingest rejected with status %d", resp.StatusCode))
		case resp.StatusCode == http.StatusTooManyRequests:
			lastErr = fmt.Errorf("ingest rate limited (429)")
			if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
				h.sleep(ra)
				attempt++
				if attempt > h.retryMax {
					break
				}

				retryBody, retryCT, bErr := h.buildMultipart(metaJSON, content, sidecars)
				if bErr != nil {
					return fmt.Errorf("build multipart body: %w", bErr)
				}
				retryReq, rErr := http.NewRequest(http.MethodPost, h.ingestURL, retryBody)
				if rErr != nil {
					return fmt.Errorf("create request: %w", rErr)
				}
				retryReq.Header.Set("Content-Type", retryCT)
				retryReq.Header.Set("X-Glovebox-Connector", h.connectorName)

				retryResp, rErr := h.httpClient.Do(retryReq)
				if rErr != nil {
					lastErr = fmt.Errorf("http request failed: %w", rErr)
					continue
				}
				drainAndClose(retryResp.Body)

				if retryResp.StatusCode == http.StatusAccepted {
					return nil
				}
				if retryResp.StatusCode == http.StatusBadRequest || retryResp.StatusCode == http.StatusRequestEntityTooLarge {
					return PermanentError(fmt.Errorf("ingest rejected with status %d", retryResp.StatusCode))
				}
				lastErr = fmt.Errorf("ingest returned status %d", retryResp.StatusCode)
				continue
			}
			continue
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("ingest returned status %d", resp.StatusCode)
			continue
		default:
			return fmt.Errorf("unexpected ingest status %d", resp.StatusCode)
		}
	}

	return fmt.Errorf("ingest failed after %d retries: %w", h.retryMax, lastErr)
}

// buildMultipart creates the multipart/form-data body with "metadata",
// "content", and zero or more "sidecar" parts (one per enrichment artifact
// produced connector-side; glovebox-afq4.12).
func (h *HTTPStagingBackend) buildMultipart(metaJSON, content []byte, sidecars []sidecarFile) (*bytes.Buffer, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	metaHeader := make(textproto.MIMEHeader)
	metaHeader.Set("Content-Disposition", `form-data; name="metadata"`)
	metaHeader.Set("Content-Type", "application/json")
	metaPart, err := w.CreatePart(metaHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := metaPart.Write(metaJSON); err != nil {
		return nil, "", err
	}

	contentHeader := make(textproto.MIMEHeader)
	contentHeader.Set("Content-Disposition", `form-data; name="content"; filename="content.raw"`)
	contentHeader.Set("Content-Type", "application/octet-stream")
	contentPart, err := w.CreatePart(contentHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := contentPart.Write(content); err != nil {
		return nil, "", err
	}

	// Enrichment sidecar artifacts (glovebox-afq4.12). Each part carries
	// the artifact's filename so the ingest handler can persist it verbatim
	// alongside content.raw.
	for _, sc := range sidecars {
		scHeader := make(textproto.MIMEHeader)
		scHeader.Set("Content-Disposition", fmt.Sprintf(`form-data; name="sidecar"; filename=%q`, sc.name))
		scHeader.Set("Content-Type", "application/octet-stream")
		scPart, err := w.CreatePart(scHeader)
		if err != nil {
			return nil, "", err
		}
		if _, err := scPart.Write(sc.data); err != nil {
			return nil, "", err
		}
	}

	if err := w.Close(); err != nil {
		return nil, "", err
	}

	return &buf, w.FormDataContentType(), nil
}

// backoffDuration computes exponential backoff with full jitter:
// base * 2^attempt * rand(0.5, 1.5)
func (h *HTTPStagingBackend) backoffDuration(attempt int) time.Duration {
	base := h.retryBase
	shift := time.Duration(1) << uint(attempt)
	d := base * shift
	jitter := 0.5 + rand.Float64()
	return time.Duration(float64(d) * jitter)
}

// sleep pauses for the given duration. Extracted for testability.
func (h *HTTPStagingBackend) sleep(d time.Duration) {
	time.Sleep(d)
}

// parseRetryAfter parses the Retry-After header value. It handles both
// delay-seconds and HTTP-date formats. Returns 0 if unparseable.
func parseRetryAfter(val string) time.Duration {
	if val == "" {
		return 0
	}
	if secs, err := strconv.Atoi(val); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(val); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}
