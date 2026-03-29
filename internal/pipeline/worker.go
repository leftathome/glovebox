package pipeline

import (
	"context"
	"sync"
	"time"

	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
)

type ScanRequest struct {
	Item    staging.StagingItem
	Matchers []engine.ScanFunc
	Detectors []engine.ScanFunc
}

type ScanResponse struct {
	Item       staging.StagingItem
	Signals    []engine.Signal
	Duration   time.Duration
	TimedOut   bool
	Err        error
}

type WorkerPool struct {
	numWorkers int
	timeout    time.Duration
	input      chan ScanRequest
	output     chan ScanResponse
}

func NewWorkerPool(numWorkers int, timeout time.Duration) *WorkerPool {
	return &WorkerPool{
		numWorkers: numWorkers,
		timeout:    timeout,
		input:      make(chan ScanRequest, numWorkers*2),
		output:     make(chan ScanResponse, numWorkers*2),
	}
}

func (p *WorkerPool) Input() chan<- ScanRequest {
	return p.input
}

func (p *WorkerPool) Output() <-chan ScanResponse {
	return p.output
}

func (p *WorkerPool) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for i := 0; i < p.numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case req, ok := <-p.input:
					if !ok {
						return
					}
					resp := p.scan(ctx, req)
					select {
					case p.output <- resp:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(p.output)
}

func (p *WorkerPool) scan(ctx context.Context, req ScanRequest) ScanResponse {
	scanCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	start := time.Now()
	done := make(chan ScanResponse, 1)

	go func() {
		f, err := openContent(req.Item.ContentPath)
		if err != nil {
			done <- ScanResponse{Item: req.Item, Err: err, Duration: time.Since(start)}
			return
		}
		defer f.Close()

		signals, err := engine.ScanContent(f, req.Matchers, req.Detectors)
		done <- ScanResponse{
			Item:     req.Item,
			Signals:  signals,
			Duration: time.Since(start),
			Err:      err,
		}
	}()

	select {
	case resp := <-done:
		return resp
	case <-scanCtx.Done():
		return ScanResponse{
			Item:     req.Item,
			Duration: time.Since(start),
			TimedOut: true,
		}
	}
}
