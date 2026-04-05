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

	"github.com/leftathome/glovebox/internal/staging"
)

// inMemoryThreshold is the maximum content size (4 MB) that will be buffered
// in memory. Above this threshold, content is spilled to a temporary file.
const inMemoryThreshold = 4 * 1024 * 1024

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
// items to ingestURL. If httpClient is nil, http.DefaultClient is used.
func NewHTTPStagingBackend(ingestURL, connectorName string, httpClient *http.Client) *HTTPStagingBackend {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &HTTPStagingBackend{
		ingestURL:     ingestURL,
		connectorName: connectorName,
		httpClient:    httpClient,
		retryMax:      3,
		retryBase:     1 * time.Second,
	}
}

// SetConfigIdentity sets the config-level identity used as the base for
// identity merging at Commit() time.
func (h *HTTPStagingBackend) SetConfigIdentity(ci *ConfigIdentity) {
	h.configIdentity = ci
}

// NewItem creates a StagingItem whose Commit() POSTs to the ingest endpoint
// instead of writing to the filesystem.
func (h *HTTPStagingBackend) NewItem(opts ItemOptions) (*StagingItem, error) {
	buf := &contentBuffer{}

	si := &StagingItem{
		opts:           opts,
		configIdentity: h.configIdentity,
	}

	// Set up a temp dir for content file operations (WriteContent/ContentWriter
	// use si.dir). We create a temp dir lazily on first write.
	tmpDir, err := os.MkdirTemp("", "glovebox-http-*")
	if err != nil {
		return nil, fmt.Errorf("create http backend tmp dir: %w", err)
	}
	si.dir = tmpDir

	si.commitFunc = func() error {
		return h.commitHTTP(si, buf, tmpDir)
	}

	return si, nil
}

// commitHTTP builds metadata, validates it, reads content from the staging
// item's directory, and POSTs the multipart request with retries.
func (h *HTTPStagingBackend) commitHTTP(si *StagingItem, buf *contentBuffer, tmpDir string) error {
	defer os.RemoveAll(tmpDir)

	// Build metadata (same logic as filesystem Commit).
	meta := staging.ItemMetadata{
		Source:           si.opts.Source,
		Sender:           si.opts.Sender,
		Subject:          staging.StripSubjectControlChars(si.opts.Subject),
		Timestamp:        si.opts.Timestamp,
		DestinationAgent: si.opts.DestinationAgent,
		ContentType:      si.opts.ContentType,
		Ordered:          si.opts.Ordered,
		AuthFailure:      si.opts.AuthFailure,
	}

	mergedTags := mergeTags(si.opts.RuleTags, si.opts.Tags)
	if len(mergedTags) > 0 {
		meta.Tags = mergedTags
	}

	mergedIdentity := MergeIdentity(si.configIdentity, si.opts.Identity)
	if mergedIdentity != nil {
		meta.Identity = &staging.ItemIdentity{
			AccountID:  mergedIdentity.AccountID,
			Provider:   mergedIdentity.Provider,
			AuthMethod: mergedIdentity.AuthMethod,
			Scopes:     mergedIdentity.Scopes,
			Tenant:     mergedIdentity.Tenant,
		}
	}

	// Validate metadata.
	if meta.DestinationAgent == "" {
		return fmt.Errorf("metadata validation: destination_agent is required")
	}
	allowlist := []string{meta.DestinationAgent}
	if errs := staging.Validate(meta, allowlist); len(errs) > 0 {
		return fmt.Errorf("metadata validation: %v", errs)
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}

	// Read content from the content.raw file that WriteContent wrote to.
	contentPath := si.dir + "/content.raw"
	contentData, err := os.ReadFile(contentPath)
	if err != nil {
		// No content written -- use empty content.
		contentData = nil
	}

	_ = buf // buf is unused; content is read from the temp file

	return h.postWithRetry(metaJSON, contentData)
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
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusAccepted:
			return nil
		case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusRequestEntityTooLarge:
			return PermanentError(fmt.Errorf("ingest rejected with status %d", resp.StatusCode))
		case resp.StatusCode == http.StatusTooManyRequests:
			lastErr = fmt.Errorf("ingest rate limited (429)")
			if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
				// Override normal backoff with server-requested delay.
				// Subtract the backoff we would have already slept (we haven't yet
				// for the next iteration). We handle this by sleeping the difference
				// now and skipping the next iteration's backoff.
				h.sleep(ra)
				// Do the retry immediately (skip the built-in backoff).
				attempt++ // consume one extra attempt slot
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
				retryResp.Body.Close()

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

	// metadata part as application/json
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

	// content part as application/octet-stream
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
	// Full jitter: multiply by random factor in [0.5, 1.5)
	jitter := 0.5 + rand.Float64() // [0.5, 1.5)
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
	// Try as seconds first.
	if secs, err := strconv.Atoi(val); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	// Try as HTTP-date.
	if t, err := http.ParseTime(val); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

// contentBuffer is a placeholder for tracking content writes. In the HTTP
// backend, content is written to the StagingItem's temp dir via the standard
// WriteContent/ContentWriter methods, then read back at Commit time.
type contentBuffer struct{}

// Ensure io.Writer conformance is not needed; content flows through StagingItem's
// existing file-based WriteContent.
var _ io.Writer = (*contentBuffer)(nil)

func (cb *contentBuffer) Write(p []byte) (int, error) {
	return len(p), nil
}
