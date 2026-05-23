// workers.go -- bounded-concurrency ingest worker pool for the mbox
// importer (spec 09 §3.9 / bead glovebox-1s3).
//
// Shape: a pool of N goroutines pulls *Message values from an input
// channel, calls a caller-supplied IngestFunc per message, and emits
// per-message WorkerResult values on an output channel. Retry,
// 4xx-vs-5xx, and 429+Retry-After handling live INSIDE the IngestFunc
// (production wires it to connector.HTTPStagingBackend whose
// postWithRetry implements all three). The pool itself is small: it
// owns goroutine lifecycle + result fan-in + ctx.Done propagation,
// nothing more.
//
// Why the split: keeping retry policy out of the pool makes both the
// pool and the IngestFunc independently testable. Tests in this
// package inject scripted IngestFuncs to exercise pool semantics;
// HTTPStagingBackend has its own tests for the retry behaviour.
package main

import (
	"context"
	"sync"
)

// WorkerStatus categorises a per-message ingest outcome at the level
// the importer's manifest cares about: counts that bump on success,
// counts that bump on failure, and the cancellation case where the
// pool never even attempted the message (so it should NOT increment
// either counter).
type WorkerStatus int

const (
	// StatusOK -- IngestFunc returned nil error; manifest bumps
	// MessagesIngested and the caller-side dedup tracker adds the
	// Message-ID.
	StatusOK WorkerStatus = iota

	// StatusFailed -- IngestFunc returned a non-nil error after all
	// internal retries; manifest bumps MessagesErrored and records
	// the per-message error entry. The pool does not distinguish
	// transient from permanent at this level; the IngestFunc is
	// responsible for deciding to retry.
	StatusFailed

	// StatusCancelled -- the context was cancelled before the worker
	// could begin IngestFunc for this message. The caller's
	// collector should NOT bump any counter for these; they belong
	// to the "drained but unprocessed" set that the next run will
	// re-attempt via dedup.
	StatusCancelled
)

// WorkerResult is one per-message outcome that flows out of the pool.
type WorkerResult struct {
	Message *Message
	Status  WorkerStatus
	// Err is the underlying error when Status==StatusFailed; nil
	// otherwise. Callers should not parse it -- IngestFunc owns the
	// error vocabulary.
	Err error
}

// IngestFunc is the per-message work the pool delegates to. It MUST
// honor ctx -- callers cancel ctx to drain the pool. In production
// this is a closure over connector.HTTPStagingBackend that calls
// NewItem -> WriteContent -> Commit. In tests it's a script that
// returns canned errors.
type IngestFunc func(ctx context.Context, msg *Message) error

// WorkerPoolConfig parameterises NewWorkerPool. Concurrency defaults
// to 1 if <= 0; QueueSize defaults to 2*Concurrency.
type WorkerPoolConfig struct {
	Concurrency int
	QueueSize   int
	Ingest      IngestFunc
}

// WorkerPool is the bounded-concurrency fan-out structure. Start it
// once; push messages on Jobs(); consume results from Results();
// close Jobs() to signal "no more work"; Wait() blocks until every
// pending message has produced exactly one WorkerResult.
//
// Concurrency cap is enforced by the fixed number of goroutines the
// pool spawns; the bead's "concurrency cap is honored" acceptance
// reduces to "we never spawn more than Concurrency workers" -- a
// structural property, validated by Test_WorkerPool_ConcurrencyCap.
type WorkerPool struct {
	cfg     WorkerPoolConfig
	jobs    chan *Message
	results chan WorkerResult
	wg      sync.WaitGroup

	// active tracks the worker count currently inside IngestFunc; the
	// concurrency-cap test reads its peak via the atomic load
	// recorded by the test's scripted IngestFunc. Not used in
	// production.
	active   *concurrencyMeter
	meterUse bool
}

// NewWorkerPool constructs an UNSTARTED pool. Call Start() to spawn
// the worker goroutines.
func NewWorkerPool(cfg WorkerPoolConfig) *WorkerPool {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 2 * cfg.Concurrency
	}
	return &WorkerPool{
		cfg:     cfg,
		jobs:    make(chan *Message, cfg.QueueSize),
		results: make(chan WorkerResult, cfg.QueueSize),
		active:  &concurrencyMeter{},
	}
}

// Jobs returns the send-side channel the producer pushes messages on.
// Closing this channel signals "no more work"; workers drain
// remaining buffered jobs, emit their results, then exit.
func (p *WorkerPool) Jobs() chan<- *Message { return p.jobs }

// Results returns the receive-side channel of per-message outcomes.
// Production callers consume this in a dedicated goroutine that
// updates the manifest + dedup tracker per WorkerResult.
func (p *WorkerPool) Results() <-chan WorkerResult { return p.results }

// Start spawns Concurrency worker goroutines. Returns immediately.
// Each worker reads from p.jobs until the channel is closed AND
// drained, then exits. Context cancellation causes workers to
// emit StatusCancelled for every remaining queued message so the
// collector can drain Results to completion without blocking.
func (p *WorkerPool) Start(ctx context.Context) {
	for i := 0; i < p.cfg.Concurrency; i++ {
		p.wg.Add(1)
		go p.workerLoop(ctx)
	}
	// Closer goroutine: when every worker has exited, close Results
	// so the collector ranges-out cleanly.
	go func() {
		p.wg.Wait()
		close(p.results)
	}()
}

// Wait blocks until every worker has exited (the closer goroutine
// has already closed Results by then). Callers that range over
// Results to natural completion do not need to call Wait.
func (p *WorkerPool) Wait() {
	p.wg.Wait()
}

func (p *WorkerPool) workerLoop(ctx context.Context) {
	defer p.wg.Done()
	for msg := range p.jobs {
		// If ctx is already cancelled, emit Cancelled and drop the
		// message rather than calling IngestFunc; the next run will
		// re-attempt via dedup. We still consume the message from
		// p.jobs so the producer's send doesn't block.
		if err := ctx.Err(); err != nil {
			p.emit(WorkerResult{Message: msg, Status: StatusCancelled, Err: err})
			continue
		}
		p.active.enter()
		err := p.cfg.Ingest(ctx, msg)
		p.active.exit()
		status := StatusOK
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				status = StatusCancelled
			} else {
				status = StatusFailed
			}
		}
		p.emit(WorkerResult{Message: msg, Status: status, Err: err})
	}
}

func (p *WorkerPool) emit(r WorkerResult) {
	// Result channel is sized to QueueSize so the producer can stay
	// ahead of a slow collector; in practice production's collector
	// is also concurrent and never blocks.
	p.results <- r
}

// concurrencyMeter records the peak count of workers inside
// IngestFunc simultaneously. Tests use this to assert the
// "concurrency cap is honored" acceptance.
type concurrencyMeter struct {
	mu      sync.Mutex
	current int
	peak    int
}

func (m *concurrencyMeter) enter() {
	m.mu.Lock()
	m.current++
	if m.current > m.peak {
		m.peak = m.current
	}
	m.mu.Unlock()
}

func (m *concurrencyMeter) exit() {
	m.mu.Lock()
	m.current--
	m.mu.Unlock()
}

// Peak returns the maximum concurrent IngestFunc count observed
// during the pool's lifetime. Used by tests; production callers can
// ignore it.
func (p *WorkerPool) Peak() int {
	p.active.mu.Lock()
	defer p.active.mu.Unlock()
	return p.active.peak
}
