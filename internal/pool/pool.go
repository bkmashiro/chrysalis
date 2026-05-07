// Package pool manages a pool of pre-compiled wazero WASM modules.
// Each Run() call instantiates a fresh module from the shared CompiledModule.
package pool

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/bkmashiro/chrysalis/internal/bridge"
	"github.com/bkmashiro/chrysalis/internal/worker"
	"github.com/tetratelabs/wazero"
)

// RunResult wraps the output of a single sandboxed execution.
type RunResult struct {
	Stdout  string
	Stderr  string
	Error   string
	Timing  Timing
}

// Timing records how long each phase took.
type Timing struct {
	TotalMs int64
	ExecMs  int64
}

// Pool holds the compiled WASM module and a pool of Python workers.
type Pool struct {
	rt         wazero.Runtime
	compiled   wazero.CompiledModule
	wkPool     *worker.Pool
	dispatcher *bridge.Dispatcher
}

// New creates a Pool by compiling the given WASM bytes and starting `workers`
// Python worker subprocesses.
//
//	wasmBytes        — content of python.wasm
//	workerScriptPath — shim/worker.py
//	shimPath         — shim/bootstrap.py
//	filterDir        — directory of YAML filter profiles
//	sockPath         — base Unix-socket path; pool members get unique suffixes
//	                   (e.g. /tmp/chrysalis_worker.sock.1, .2, …)
//	workers          — pool size (>= 1)
func New(ctx context.Context, wasmBytes []byte, workerScriptPath, shimPath, filterDir, sockPath string, workers int) (*Pool, error) {
	// Compile the WASM module once.
	// CloseOnContextDone makes wazero close in-flight modules when their
	// per-request context is cancelled (e.g. the user's timeout_sec elapses).
	// Without it, a tight WASM loop with no host-call yield points runs
	// forever; with it, the next host-function call observes ctx.Err() and
	// the module exits promptly.
	rt := wazero.NewRuntimeWithConfig(ctx,
		wazero.NewRuntimeConfig().WithCloseOnContextDone(true),
	)

	if err := bridge.InstantiateWASI(ctx, rt); err != nil {
		return nil, fmt.Errorf("instantiate WASI: %w", err)
	}

	log.Println("chrysalis: compiling WASM module (this may take 10-60s)…")
	t0 := time.Now()
	compiled, err := rt.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile WASM: %w", err)
	}
	log.Printf("chrysalis: WASM compiled in %v", time.Since(t0))

	// Read bootstrap shim.
	shimCode, err := os.ReadFile(shimPath)
	if err != nil {
		return nil, fmt.Errorf("read shim %s: %w", shimPath, err)
	}

	// Start the worker pool — N processes in parallel.
	log.Printf("chrysalis: starting worker pool (size=%d)…", workers)
	wkPool, err := worker.NewPool(ctx, workers, sockPath, workerScriptPath)
	if err != nil {
		return nil, fmt.Errorf("start worker pool: %w", err)
	}

	disp := bridge.NewDispatcher(rt, compiled, wkPool, shimCode, filterDir)

	return &Pool{
		rt:         rt,
		compiled:   compiled,
		wkPool:     wkPool,
		dispatcher: disp,
	}, nil
}

// Run executes user code in a fresh WASM instance.
func (p *Pool) Run(ctx context.Context, code, filterProfile string, timeoutSec int) (*RunResult, error) {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()

	t0 := time.Now()
	res, err := p.dispatcher.Run(ctx, code, filterProfile, timeoutSec)
	elapsed := time.Since(t0)

	if err != nil {
		return nil, err
	}

	return &RunResult{
		Stdout: res.Stdout,
		Stderr: res.Stderr,
		Error:  res.Error,
		Timing: Timing{
			TotalMs: elapsed.Milliseconds(),
			ExecMs:  elapsed.Milliseconds(),
		},
	}, nil
}

// Close shuts down the runtime and the worker pool.
func (p *Pool) Close(ctx context.Context) {
	if p.wkPool != nil {
		p.wkPool.Close()
	}
	_ = p.rt.Close(ctx)
}
