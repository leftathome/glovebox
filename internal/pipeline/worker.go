package pipeline

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/scan"
	"github.com/leftathome/glovebox/internal/staging"
)

type ScanRequest struct {
	Item staging.StagingItem
}

type ScanResponse struct {
	Item     staging.StagingItem
	Signals  []engine.Signal
	Result   *engine.ScanResult
	Duration time.Duration
	TimedOut bool
	Err      error
}

type WorkerPool struct {
	numWorkers int
	timeout    time.Duration
	scanner    *scan.Scanner
	input      chan ScanRequest
	output     chan ScanResponse
}

func NewWorkerPool(numWorkers int, timeout time.Duration, scanner *scan.Scanner) *WorkerPool {
	return &WorkerPool{
		numWorkers: numWorkers,
		timeout:    timeout,
		scanner:    scanner,
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
		rawContent, err := os.ReadFile(req.Item.ContentPath)
		if err != nil {
			done <- ScanResponse{Item: req.Item, Err: err, Duration: time.Since(start)}
			return
		}

		res, err := p.scanner.Scan(rawContent, req.Item.Metadata.ContentType)
		if err != nil {
			done <- ScanResponse{Item: req.Item, Err: err, Duration: time.Since(start)}
			return
		}
		done <- ScanResponse{Item: req.Item, Signals: res.Signals, Result: &res, Duration: time.Since(start)}
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
