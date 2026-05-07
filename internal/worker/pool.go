// Package worker — pool.go provides a fixed-size pool of Python workers.
//
// Each /run request acquires one worker for its full duration (so per-request
// state like RNG seeds stays consistent across the request's bridge calls),
// and returns it on completion. The pool serialises by worker, not globally —
// N concurrent requests can run against N workers in parallel.
package worker

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// Pool is a fixed-size set of long-running Python worker processes.
//
// Acquire/Release follow the standard "return a release function from
// Acquire" idiom so callers cannot forget to return the worker:
//
//	wk, release, err := pool.Acquire(ctx)
//	if err != nil { return err }
//	defer release()
//	... use wk ...
//
// Released workers go back into a buffered channel; Acquire pulls from it.
// If a worker is unhealthy on acquire, it is replaced before being returned.
type Pool struct {
	workers      chan *Worker
	sockDir      string
	sockBaseName string
	workerScript string
	nextSockID   atomic.Uint64
	closeOnce    sync.Once
	closed       atomic.Bool
}

// NewPool starts `size` workers in parallel and returns a Pool when all are
// ready. If any fails to start, all are torn down.
func NewPool(ctx context.Context, size int, sockPath, workerScript string) (*Pool, error) {
	if size < 1 {
		return nil, fmt.Errorf("worker pool size must be >= 1, got %d", size)
	}

	sockDir, sockBaseName := filepath.Split(sockPath)
	if sockDir == "" {
		sockDir = "."
	}

	p := &Pool{
		workers:      make(chan *Worker, size),
		sockDir:      sockDir,
		sockBaseName: sockBaseName,
		workerScript: workerScript,
	}

	type startResult struct {
		wk  *Worker
		err error
	}
	results := make(chan startResult, size)
	for i := 0; i < size; i++ {
		go func() {
			wk, err := p.spawn()
			results <- startResult{wk: wk, err: err}
		}()
	}

	started := make([]*Worker, 0, size)
	var firstErr error
	for i := 0; i < size; i++ {
		r := <-results
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		started = append(started, r.wk)
	}

	if firstErr != nil {
		for _, wk := range started {
			wk.Shutdown()
		}
		return nil, fmt.Errorf("worker pool: failed to start all %d workers: %w", size, firstErr)
	}

	for _, wk := range started {
		p.workers <- wk
	}

	log.Printf("chrysalis: worker pool ready (size=%d)", size)
	return p, nil
}

// Acquire blocks until a worker is available or ctx is cancelled.
// Unhealthy workers are transparently replaced before being returned.
// The returned release function MUST be called exactly once.
func (p *Pool) Acquire(ctx context.Context) (*Worker, func(), error) {
	if p.closed.Load() {
		return nil, nil, fmt.Errorf("worker pool: closed")
	}

	const maxReplaceAttempts = 3
	for attempt := 0; attempt < maxReplaceAttempts; attempt++ {
		var wk *Worker
		select {
		case wk = <-p.workers:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}

		// Health check before handing out. A short ping confirms the worker
		// is still serving on its socket; dead workers are replaced.
		if err := wk.Ping(); err != nil {
			log.Printf("chrysalis: worker pid=%d unhealthy on acquire (%v) — replacing", wk.cmd.Process.Pid, err)
			wk.Shutdown()
			replacement, sErr := p.spawn()
			if sErr != nil {
				return nil, nil, fmt.Errorf("worker pool: replacement spawn failed: %w", sErr)
			}
			wk = replacement
		}

		release := func() { p.release(wk) }
		return wk, release, nil
	}
	return nil, nil, fmt.Errorf("worker pool: unable to obtain healthy worker after %d attempts", maxReplaceAttempts)
}

// release returns a worker to the pool. If the pool is closed, the worker is
// shut down instead.
func (p *Pool) release(wk *Worker) {
	if p.closed.Load() {
		wk.Shutdown()
		return
	}
	select {
	case p.workers <- wk:
	default:
		// Should not happen — channel is sized to the number of workers we
		// ever own. If it does, prefer leaking a slot to deadlocking.
		log.Printf("chrysalis: worker pool full on release (slot leak); shutting down worker")
		wk.Shutdown()
	}
}

// Close shuts down every worker the pool currently owns. It blocks until all
// of them have exited. Idempotent.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		// Drain the channel; we own everything currently in it.
		// Workers that are out (acquired) will be shut down on release().
		close(p.workers)
		for wk := range p.workers {
			wk.Shutdown()
		}
	})
}

// spawn starts a new worker on a unique socket path under the pool's sock dir.
func (p *Pool) spawn() (*Worker, error) {
	id := p.nextSockID.Add(1)
	sockPath := filepath.Join(p.sockDir, fmt.Sprintf("%s.%d", p.sockBaseName, id))
	return Start(sockPath, p.workerScript)
}
