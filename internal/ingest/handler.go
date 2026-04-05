package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/leftathome/glovebox/internal/config"
	"github.com/leftathome/glovebox/internal/staging"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Handler serves the POST /v1/ingest endpoint for the scanner's HTTP ingest API.
type Handler struct {
	stagingDir string
	config     config.IngestConfig
	allowlist  []string
	queueDepth atomic.Int64
	ready      atomic.Bool
	metrics    *IngestMetrics
}

// SetMetrics attaches ingest metrics to the handler. If not called (or called
// with nil), the handler operates without recording metrics.
func (h *Handler) SetMetrics(m *IngestMetrics) {
	h.metrics = m
}

// NewHandler creates a new ingest handler.
func NewHandler(stagingDir string, cfg config.IngestConfig, allowlist []string) *Handler {
	return &Handler{
		stagingDir: stagingDir,
		config:     cfg,
		allowlist:  allowlist,
	}
}

// SetReady marks the handler as ready to accept requests.
func (h *Handler) SetReady() {
	h.ready.Store(true)
}

// DecrementQueue decrements the backpressure counter when the scanner watcher
// dispatches an item from the staging directory to the scan worker pool.
func (h *Handler) DecrementQueue() {
	h.queueDepth.Add(-1)
}

// InitQueueDepth counts existing non-hidden directories in stagingDir and
// removes the .ingest-tmp/ directory if it exists (orphan cleanup).
func (h *Handler) InitQueueDepth() error {
	// Clean up orphaned temp directory
	ingestTmp := filepath.Join(h.stagingDir, ".ingest-tmp")
	if err := os.RemoveAll(ingestTmp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clean .ingest-tmp: %w", err)
	}

	entries, err := os.ReadDir(h.stagingDir)
	if err != nil {
		return fmt.Errorf("read staging dir: %w", err)
	}

	var count int64
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			count++
		}
	}
	h.queueDepth.Store(count)
	return nil
}

// recordReceived increments the items_received counter if metrics are wired.
func (h *Handler) recordReceived(source, status string) {
	if h.metrics == nil {
		return
	}
	h.metrics.itemsReceived.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("source", source),
			attribute.String("status", status),
		))
}

// recordAcceptMetrics records duration, bytes, and staging depth on a
// successful ingest (202).
func (h *Handler) recordAcceptMetrics(source string, contentSize int64, elapsed time.Duration) {
	if h.metrics == nil {
		return
	}
	ctx := context.Background()
	srcAttr := metric.WithAttributes(attribute.String("source", source))
	h.metrics.receiveDuration.Record(ctx, elapsed.Seconds(), srcAttr)
	h.metrics.receiveBytes.Add(ctx, contentSize, srcAttr)
}

// recordStagingDepth records the current staging queue depth gauge.
func (h *Handler) recordStagingDepth() {
	if h.metrics == nil {
		return
	}
	h.metrics.stagingDepth.Record(context.Background(), h.queueDepth.Load())
}

// ServeHTTP handles ingest requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Readiness gate
	if !h.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status":  "unavailable",
			"message": "scanner not ready",
		})
		return
	}

	// Method check
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"status":  "error",
			"message": "method not allowed",
		})
		return
	}

	// Backpressure check
	if h.queueDepth.Load() >= int64(h.config.BackpressureThreshold) {
		h.recordReceived("", "throttled")
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusTooManyRequests, map[string]interface{}{
			"status":              "backpressure",
			"retry_after_seconds": 5,
		})
		return
	}

	// Parse multipart form with body size limit
	limitedBody := http.MaxBytesReader(w, r.Body, h.config.MaxBodyBytes)

	contentType := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		h.recordReceived("", "rejected")
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"status":  "error",
			"message": "expected multipart form data",
		})
		return
	}

	reader := multipart.NewReader(limitedBody, params["boundary"])

	// Read exactly 2 parts: metadata and content
	var metadataBytes []byte
	var contentBytes []byte
	var hasMetadata, hasContent bool
	partCount := 0

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Could be a max bytes error
			if isMaxBytesError(err) {
				h.recordReceived("", "rejected")
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
					"status":  "error",
					"message": "request body too large",
				})
				return
			}
			h.recordReceived("", "rejected")
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"status":  "error",
				"message": fmt.Sprintf("parse multipart: %v", err),
			})
			return
		}

		partName := part.FormName()
		partCount++

		switch partName {
		case "metadata":
			if hasMetadata {
				part.Close()
				h.recordReceived("", "rejected")
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"status":  "error",
					"message": "duplicate metadata part",
				})
				return
			}
			// Check content-type on metadata part
			partCT := part.Header.Get("Content-Type")
			parsedCT, _, _ := mime.ParseMediaType(partCT)
			if parsedCT != "application/json" {
				part.Close()
				h.recordReceived("", "rejected")
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"status":  "error",
					"message": fmt.Sprintf("metadata part must be application/json, got %q", partCT),
				})
				return
			}
			// Read metadata with size limit
			limitedMeta := io.LimitReader(part, h.config.MaxMetadataBytes+1)
			metadataBytes, err = io.ReadAll(limitedMeta)
			if err != nil {
				part.Close()
				if isMaxBytesError(err) {
					h.recordReceived("", "rejected")
					writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
						"status":  "error",
						"message": "request body too large",
					})
					return
				}
				h.recordReceived("", "rejected")
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"status":  "error",
					"message": fmt.Sprintf("read metadata: %v", err),
				})
				return
			}
			if int64(len(metadataBytes)) > h.config.MaxMetadataBytes {
				part.Close()
				h.recordReceived("", "rejected")
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
					"status":  "error",
					"message": "metadata too large",
				})
				return
			}
			hasMetadata = true
			part.Close()

		case "content":
			if hasContent {
				part.Close()
				h.recordReceived("", "rejected")
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"status":  "error",
					"message": "duplicate content part",
				})
				return
			}
			contentBytes, err = io.ReadAll(part)
			if err != nil {
				part.Close()
				if isMaxBytesError(err) {
					h.recordReceived("", "rejected")
					writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
						"status":  "error",
						"message": "request body too large",
					})
					return
				}
				h.recordReceived("", "rejected")
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"status":  "error",
					"message": fmt.Sprintf("read content: %v", err),
				})
				return
			}
			hasContent = true
			part.Close()

		default:
			part.Close()
			h.recordReceived("", "rejected")
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"status":  "error",
				"message": fmt.Sprintf("unexpected part: %q", partName),
			})
			return
		}
	}

	if !hasMetadata {
		h.recordReceived("", "rejected")
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"status":  "error",
			"message": "missing metadata part",
		})
		return
	}
	if !hasContent {
		h.recordReceived("", "rejected")
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"status":  "error",
			"message": "missing content part",
		})
		return
	}

	// Parse and validate metadata
	meta, err := staging.ParseMetadata(bytes.NewReader(metadataBytes))
	if err != nil {
		h.recordReceived("", "rejected")
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"status":  "error",
			"message": fmt.Sprintf("invalid metadata JSON: %v", err),
		})
		return
	}

	if errs := staging.Validate(meta, h.allowlist); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		h.recordReceived(meta.Source, "rejected")
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"status":  "error",
			"message": fmt.Sprintf("metadata validation failed: %s", strings.Join(msgs, "; ")),
		})
		return
	}

	// Create temp dir for atomic write
	itemName := fmt.Sprintf("%s-%s",
		time.Now().UTC().Format("20060102-150405"),
		uuid.New().String()[:8])
	ingestTmpDir := filepath.Join(h.stagingDir, ".ingest-tmp")
	if err := os.MkdirAll(ingestTmpDir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": "internal error",
		})
		return
	}
	tmpItemDir := filepath.Join(ingestTmpDir, itemName)
	if err := os.MkdirAll(tmpItemDir, 0755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": "internal error",
		})
		return
	}

	// cleanup helper for error paths
	cleanup := func() {
		os.RemoveAll(tmpItemDir)
	}

	// Write content.raw
	if err := os.WriteFile(filepath.Join(tmpItemDir, "content.raw"), contentBytes, 0644); err != nil {
		cleanup()
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": "internal error",
		})
		return
	}

	// Write metadata.json
	if err := os.WriteFile(filepath.Join(tmpItemDir, "metadata.json"), metadataBytes, 0644); err != nil {
		cleanup()
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": "internal error",
		})
		return
	}

	// Atomic rename into staging dir
	destDir := filepath.Join(h.stagingDir, itemName)
	if err := os.Rename(tmpItemDir, destDir); err != nil {
		cleanup()
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": "internal error",
		})
		return
	}

	// Increment backpressure counter
	h.queueDepth.Add(1)
	h.recordStagingDepth()

	// Record metrics and return 202
	elapsed := time.Since(start)
	h.recordReceived(meta.Source, "accepted")
	h.recordAcceptMetrics(meta.Source, int64(len(contentBytes)), elapsed)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "accepted",
		"item_id": itemName,
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func isMaxBytesError(err error) bool {
	// http.MaxBytesError was added in Go 1.19.
	// Also check the string representation for compatibility.
	if err == nil {
		return false
	}
	if _, ok := err.(*http.MaxBytesError); ok {
		return true
	}
	return strings.Contains(err.Error(), "http: request body too large")
}
