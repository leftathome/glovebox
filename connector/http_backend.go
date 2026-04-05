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
	"strconv"
	"time"
)

// HTTPStagingBackend implements StagingBackend by POSTing items to the
// scanner's /v1/ingest endpoint as multipart/form-data requests.
type HTTPStagingBackend struct {
	ingestURL      string
	connectorName  string
	httpClient     *http.Client
	retryMax       int
	retryBase      time.Duration
	configIdentity *ConfigIdentity
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

// NewItem creates a StagingItem whose Commit() POSTs to the ingest endpoint
// instead of writing to the filesystem.
func (h *HTTPStagingBackend) NewItem(opts ItemOptions) (*StagingItem, error) {
	si := &StagingItem{
		opts:           opts,
		configIdentity: h.configIdentity,
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

// commitHTTP builds metadata, validates it, reads content from the staging
// item's temp directory, and POSTs the multipart request with retries.
func (h *HTTPStagingBackend) commitHTTP(si *StagingItem, tmpDir string) error {
	defer os.RemoveAll(tmpDir)

	meta, err := si.buildMetadata()
	if err != nil {
		return err
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	contentPath := si.dir + "/content.raw"
	contentData, err := os.ReadFile(contentPath)
	if err != nil {
		contentData = nil
	}

	return h.postWithRetry(metaJSON, contentData)
}

// drainAndClose reads remaining bytes from the response body and closes it,
// allowing HTTP connection reuse.
func drainAndClose(body io.ReadCloser) {
	io.Copy(io.Discard, body)
	body.Close()
}

// postWithRetry sends the multipart request and retries on transient errors.
func (h *HTTPStagingBackend) postWithRetry(metaJSON, content []byte) error {
	var lastErr error

	for attempt := 0; attempt <= h.retryMax; attempt++ {
		if attempt > 0 {
			h.sleep(h.backoffDuration(attempt - 1))
		}

		body, contentType, err := h.buildMultipart(metaJSON, content)
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

				retryBody, retryCT, bErr := h.buildMultipart(metaJSON, content)
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

// buildMultipart creates the multipart/form-data body with "metadata" and
// "content" parts.
func (h *HTTPStagingBackend) buildMultipart(metaJSON, content []byte) (*bytes.Buffer, string, error) {
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
