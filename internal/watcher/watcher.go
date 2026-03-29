package watcher

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/fsnotify/fsnotify"
)

type ItemHandler func(dirPath string)

type Watcher struct {
	stagingDir   string
	pollInterval time.Duration
	handler      ItemHandler
	seen         map[string]bool
}

func New(stagingDir string, pollInterval time.Duration, handler ItemHandler) *Watcher {
	return &Watcher{
		stagingDir:   stagingDir,
		pollInterval: pollInterval,
		handler:      handler,
		seen:         make(map[string]bool),
	}
}

func (w *Watcher) Run(ctx context.Context) {
	// Try fsnotify first, fall back to polling
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

	// Process any items already in staging
	w.pollOnce()

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-fw.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				w.processIfDir(event.Name)
			}
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
	entries, err := os.ReadDir(w.stagingDir)
	if err != nil {
		log.Printf("poll staging dir: %v", err)
		return
	}

	// Sort by name for FIFO (timestamp-prefixed directories)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(w.stagingDir, e.Name())
		if w.seen[path] {
			continue
		}
		w.seen[path] = true
		w.handler(path)
	}
}

func (w *Watcher) processIfDir(path string) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return
	}
	if w.seen[path] {
		return
	}
	w.seen[path] = true
	w.handler(path)
}
