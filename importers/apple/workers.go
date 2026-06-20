// workers.go -- minimal bounded-concurrency staging pool for the apple
// importer.
//
// This mirrors the walhelm importer's worker-pool shape (importers/walhelm/
// workers.go): the job unit is a tree-file path. The pool owns goroutine
// lifecycle, error fan-in, and ctx-cancellation propagation; the actual
// per-file work (BuildItemOptions -> NewItem -> WriteContent -> Commit) lives
// in the caller-supplied stageFunc.
//
// Retry / 4xx-vs-5xx / 429 handling are NOT here -- in production those live
// inside connector's HTTPStagingBackend, reached through the stageFunc. The
// pool stays small so both it and the stageFunc are independently testable.
package main

import (
	"context"
	"sync"
)

// stageFunc is the per-file work the pool delegates to. It MUST honor ctx so
// the caller can cancel an in-flight import. rel is the tree-relative path of
// the file (used only for error attribution by the caller's closure).
type stageFunc func(ctx context.Context, rel string) error

// stageResult is one per-file outcome flowing out of the pool. Err is nil on
// success.
type stageResult struct {
	RelPath string
	Err     error
}

// stageAll runs fn over every path in jobs with at most concurrency goroutines
// active at once, collecting one stageResult per job. It returns when every
// job has produced a result.
//
// Cancellation: if ctx is cancelled, workers stop pulling new jobs;
// already-running fn calls run to completion (fn is expected to observe ctx
// itself). Jobs not started after cancellation produce a result carrying
// ctx.Err() so the caller's error tally is complete.
//
// concurrency <= 0 is treated as 1.
func stageAll(ctx context.Context, concurrency int, jobs []string, fn stageFunc) []stageResult {
	if concurrency <= 0 {
		concurrency = 1
	}

	results := make([]stageResult, len(jobs))

	// indexFor maps job paths back to their result slot. Paths can repeat in
	// theory; to keep the mapping unambiguous we feed indices, not bare paths,
	// through the channel.
	type indexed struct {
		i   int
		rel string
	}
	idxCh := make(chan indexed)

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range idxCh {
				if err := ctx.Err(); err != nil {
					results[job.i] = stageResult{RelPath: job.rel, Err: err}
					continue
				}
				err := fn(ctx, job.rel)
				results[job.i] = stageResult{RelPath: job.rel, Err: err}
			}
		}()
	}

	// Producer: feed indexed jobs, stop early if ctx is cancelled. We still
	// must close idxCh and drain the remaining slots so the result slice has
	// no zero-value holes.
	go func() {
		defer close(idxCh)
		for i, rel := range jobs {
			select {
			case <-ctx.Done():
				// Fill the rest with cancellation results directly; the
				// workers won't see these jobs.
				for j := i; j < len(jobs); j++ {
					results[j] = stageResult{RelPath: jobs[j], Err: ctx.Err()}
				}
				return
			case idxCh <- indexed{i: i, rel: rel}:
			}
		}
	}()

	wg.Wait()
	return results
}
