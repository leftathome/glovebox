package watcher

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type ItemHandler func(dirPath string)

// dispatchBuffer decouples discovery from dispatch. Discovered item paths are
// queued here; a dedicated feeder goroutine drains the queue into the handler
// (which performs the blocking scan-pool send). The buffer only needs to be
// large enough to keep the feeder fed between polls -- when it fills (the pool
// is saturated), discovery simply leaves items unseen and a later poll retries
// them, so nothing is lost and discovery never blocks.
const dispatchBuffer = 256

type Watcher struct {
	stagingDir   string
	pollInterval time.Duration
	handler      ItemHandler
	seen         map[string]bool
	dispatch     chan string
	feederOnce   sync.Once
}

func New(stagingDir string, pollInterval time.Duration, handler ItemHandler) *Watcher {
	return &Watcher{
		stagingDir:   stagingDir,
		pollInterval: pollInterval,
		handler:      handler,
		seen:         make(map[string]bool),
		dispatch:     make(chan string, dispatchBuffer),
	}
}

// startFeeder launches the single feeder goroutine that drains the dispatch
// queue into the handler. The handler's pool send may block under load; because
// it runs here -- NOT on the discovery goroutine -- a saturated pool can never
// freeze fsnotify-event draining or polling (glovebox-rbpt). Idempotent: safe to
// call from both Run and runPolling (incl. the fsnotify->polling fallback).
func (w *Watcher) startFeeder(ctx context.Context) {
	w.feederOnce.Do(func() {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case path := <-w.dispatch:
					w.handler(path)
				}
			}
		}()
	})
}

func (w *Watcher) Run(ctx context.Context) {
	w.startFeeder(ctx)

	fw, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("fsnotify unavailable, falling back to polling: %v", err)
		w.runPolling(ctx)
		return
	}
	defer fw.Close()

	if err := fw.Add(w.stagingDir); err != nil {
		log.Printf("fsnotify watch failed, falling back to polling: %v", err)
		w.runPolling(ctx)
		return
	}

	w.pollOnce()

	// Periodic poll alongside fsnotify catches items whose metadata.json
	// was not yet visible when the fsnotify Create event fired, and retries
	// any items that could not be enqueued while the dispatch queue was full.
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-fw.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				info, err := os.Stat(event.Name)
				if err != nil || !info.IsDir() {
					continue
				}
				w.dispatchIfNew(event.Name)
			}
		case <-ticker.C:
			w.pollOnce()
		case err, ok := <-fw.Errors:
			if !ok {
				return
			}
			log.Printf("fsnotify error, falling back to polling: %v", err)
			w.runPolling(ctx)
			return
		}
	}
}

func (w *Watcher) runPolling(ctx context.Context) {
	w.startFeeder(ctx)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	w.pollOnce()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce()
		}
	}
}

func (w *Watcher) pollOnce() {
	w.pruneSeen()

	entries, err := os.ReadDir(w.stagingDir)
	if err != nil {
		log.Printf("poll staging dir: %v", err)
		return
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		w.dispatchIfNew(filepath.Join(w.stagingDir, e.Name()))
	}
}

// dispatchIfNew queues a not-yet-seen, ready item for dispatch. It runs on the
// discovery goroutine and MUST NOT block: the enqueue is non-blocking, and an
// item that does not fit (dispatch queue full because the scan pool is
// saturated) is deliberately left unseen so the next pollOnce retries it. This
// is what keeps discovery -- and therefore fsnotify-event draining -- alive
// under backpressure (glovebox-rbpt).
func (w *Watcher) dispatchIfNew(path string) {
	if w.seen[path] {
		return
	}
	// Readiness gate: metadata.json is the last file written by the connector's
	// Commit(). Its presence guarantees content.raw also exists. This is critical
	// for networked/virtualized mounts (NFS, iSCSI, 9P, virtiofs) where directory
	// contents may not be visible immediately after an atomic rename.
	if _, err := os.Stat(filepath.Join(path, "metadata.json")); err != nil {
		return
	}
	// Only mark seen once it is actually queued. If the queue is full, leaving
	// it unseen lets a later poll retry it -- no item is dropped.
	select {
	case w.dispatch <- path:
		w.seen[path] = true
	default:
	}
}

// pruneSeen removes entries for paths that no longer exist on disk.
func (w *Watcher) pruneSeen() {
	for path := range w.seen {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			delete(w.seen, path)
		}
	}
}
