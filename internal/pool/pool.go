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

// Pool holds the compiled WASM module and dispatches execution requests.
type Pool struct {
	rt         wazero.Runtime
	compiled   wazero.CompiledModule
	dispatcher *bridge.Dispatcher
}

// New creates a Pool by compiling the given WASM bytes and starting a Python worker.
// wasmBytes is the content of python.wasm.
// workerPath is the path to shim/worker.py.
// shimPath is the path to shim/bootstrap.py.
// filterDir is the directory containing YAML filter profiles.
// sockPath is where the Unix socket for the Python worker will live.
func New(ctx context.Context, wasmBytes []byte, workerScriptPath, shimPath, filterDir, sockPath string) (*Pool, error) {
	// Compile the WASM module once.
	rt := wazero.NewRuntime(ctx)

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

	// Start the Python worker.
	log.Println("chrysalis: starting Python worker…")
	wk, err := worker.Start(sockPath, workerScriptPath)
	if err != nil {
		return nil, fmt.Errorf("start worker: %w", err)
	}

	disp := bridge.NewDispatcher(rt, compiled, wk, shimCode, filterDir)

	return &Pool{
		rt:         rt,
		compiled:   compiled,
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

// Close shuts down the runtime and worker.
func (p *Pool) Close(ctx context.Context) {
	_ = p.rt.Close(ctx)
}
