// Package bridge implements the wazero host function dispatcher and the
// stdin/stdout bridge protocol used to communicate between Go and CPython-WASI.
package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"

	"os"

	"github.com/bkmashiro/chrysalis/internal/filter"
	"github.com/bkmashiro/chrysalis/internal/worker"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// BridgeRequest is the JSON sent by bootstrap.py over the bridge pipe.
type BridgeRequest struct {
	Fn     string          `json:"fn"`
	Args   []worker.Arg    `json:"args"`
	Kwargs map[string]worker.Arg `json:"kwargs"`
}

// BridgeResponse is the JSON sent back to bootstrap.py over the bridge pipe.
type BridgeResponse struct {
	Value *worker.Arg `json:"value,omitempty"`
	Error string      `json:"error,omitempty"`
}

// RunResult contains the output of a WASM-sandboxed execution.
type RunResult struct {
	Stdout string
	Stderr string
	Error  string
}

// Dispatcher holds a compiled CPython-WASI module and can execute user code
// by instantiating fresh WASM modules per request.
type Dispatcher struct {
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	wkPool   *worker.Pool
	shimCode []byte // content of bootstrap.py
	filterDir string
	// probeCache memoises *.__probe__ responses across all requests AND across
	// every worker in the pool — probes are deterministic given a fixed Python
	// env, so the answer is the same regardless of which pool member would
	// have served it. Eliminates one round-trip per first-touch attribute
	// access (e.g. np.X).
	// Key: target string (e.g. "numpy.dot"). Value: probeResult.
	probeCache sync.Map
}

// probeResult is the cached output of a worker probe call.
type probeResult struct {
	IsModule   bool
	IsCallable bool
}

// NewDispatcher creates a Dispatcher from a compiled WASM module.
func NewDispatcher(rt wazero.Runtime, compiled wazero.CompiledModule, wkPool *worker.Pool, shimCode []byte, filterDir string) *Dispatcher {
	return &Dispatcher{
		rt:        rt,
		compiled:  compiled,
		wkPool:    wkPool,
		shimCode:  shimCode,
		filterDir: filterDir,
	}
}

// Run executes user code in a fresh WASM instance.
// code is the user Python, filterProfile is the YAML profile name.
func (d *Dispatcher) Run(ctx context.Context, code string, filterProfile string, timeoutSec int) (*RunResult, error) {
	var f *filter.Filter
	var err error
	if filterProfile == "" || filterProfile == "allow-all" {
		f = filter.AllowAll()
	} else {
		f, err = filter.LoadFilter(d.filterDir, filterProfile)
		if err != nil {
			return nil, fmt.Errorf("load filter: %w", err)
		}
	}

	// Acquire one worker for the entire request. All bridge calls in this
	// /run hit the same worker so any per-request worker state (e.g. RNG
	// seed if user does numpy.random.seed(...)) is consistent.
	wk, releaseWorker, err := d.wkPool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire worker: %w", err)
	}
	defer releaseWorker()

	// Pipes for the stdin/stdout bridge.
	// bootstrap.py writes bridge requests to its stdout; Go reads from pipeStdoutR.
	// Go writes bridge responses to pipeStdinW; bootstrap.py reads from its stdin.
	pipeStdinR, pipeStdinW := io.Pipe()   // Go → WASM stdin  (responses)
	pipeStdoutR, pipeStdoutW := io.Pipe() // WASM stdout → Go (requests)

	// Stderr is captured separately.
	var stderrBuf bytes.Buffer

	// The captured user stdout is accumulated here (bootstrap.py redirects print() internally).
	// We read it from a special "__stdout__" field in the final message.
	var stdoutBuf bytes.Buffer
	var stdoutMu sync.Mutex

	// Build the full script: inject bootstrap then user code.
	fullScript := string(d.shimCode) + "\n\n# ---- user code ----\n" + code + "\n\n# ---- flush ----\n" +
		"import sys as _sys\n_sys.stdout.flush()\n"

	// Goroutine: bridge dispatcher — reads requests from pipeStdoutR, dispatches to worker.
	bridgeDone := make(chan struct{})
	go func() {
		defer close(bridgeDone)
		defer pipeStdinW.Close() // signal EOF to WASM stdin when done
		for {
			// Read 4-byte big-endian length prefix.
			hdr := make([]byte, 4)
			if _, err := io.ReadFull(pipeStdoutR, hdr); err != nil {
				if err != io.EOF && err != io.ErrClosedPipe {
					log.Printf("bridge: read header: %v", err)
				}
				return
			}
			n := binary.BigEndian.Uint32(hdr)
			if n == 0 {
				return
			}
			if n > 8*1024*1024 {
				log.Printf("bridge: oversized message %d", n)
				return
			}
			body := make([]byte, n)
			if _, err := io.ReadFull(pipeStdoutR, body); err != nil {
				log.Printf("bridge: read body: %v", err)
				return
			}

			// Check for the special "__stdout__" sentinel.
			if bytes.HasPrefix(body, []byte(`{"__stdout__"`)) {
				var envelope struct {
					Stdout string `json:"__stdout__"`
				}
				if je := json.Unmarshal(body, &envelope); je == nil {
					stdoutMu.Lock()
					stdoutBuf.WriteString(envelope.Stdout)
					stdoutMu.Unlock()
					// Send an empty ACK so the WASM side doesn't block.
					sendBridgeResponse(pipeStdinW, &BridgeResponse{})
					continue
				}
			}

			var req BridgeRequest
			if jerr := json.Unmarshal(body, &req); jerr != nil {
				log.Printf("bridge: unmarshal request: %v", jerr)
				sendBridgeResponse(pipeStdinW, &BridgeResponse{Error: "bad request: " + jerr.Error()})
				continue
			}

			resp := d.dispatch(ctx, wk, f, &req)
			sendBridgeResponse(pipeStdinW, resp)
		}
	}()

	// Configure and instantiate the WASM module.
	mc := wazero.NewModuleConfig().
		WithName(""). // allow multiple instances without name collision
		WithStdin(pipeStdinR).
		WithStdout(pipeStdoutW).
		WithStderr(&stderrBuf).
		WithArgs("python3", "-c", fullScript).
		WithEnv("PYTHONDONTWRITEBYTECODE", "1").
		WithEnv("PYTHONUNBUFFERED", "1")

	_, execErr := d.rt.InstantiateModule(ctx, d.compiled, mc)

	// Wake up the bridge goroutine so it can exit.
	//
	// stdoutW.Close() unblocks its next pipeStdoutR.Read with EOF — that's
	// how the goroutine learns there are no more bridge requests coming.
	//
	// stdinR.Close() is the subtle one: if wazero exited mid-cycle (e.g.
	// timeout on a tight bridge-call loop), the goroutine may currently be
	// blocked in sendBridgeResponse → pipeStdinW.Write, waiting for a WASM
	// reader that no longer exists. Closing the read end of that pipe
	// returns ErrClosedPipe to the writer and lets the goroutine unstick.
	pipeStdoutW.Close()
	pipeStdinR.Close()
	<-bridgeDone

	result := &RunResult{
		Stderr: stderrBuf.String(),
	}

	stdoutMu.Lock()
	result.Stdout = stdoutBuf.String()
	stdoutMu.Unlock()

	if execErr != nil {
		// Filter out normal exit codes — CPython exits 0 on success.
		errStr := execErr.Error()
		if !strings.Contains(errStr, "exit_code(0)") {
			result.Error = errStr
		}
	}

	return result, nil
}

// dispatch handles a single bridge request from the WASM side. The worker
// argument is the one acquired for this /run; all worker-bound traffic for
// this request goes through it.
//
// The shm manager is per-call: shm files are only needed to forward ndarray
// args from WASM (where we cannot allocate /dev/shm) to the worker (which
// reads them by path). Once the worker returns, those files are useless and
// freeing them per-call keeps tmpfs pressure bounded — a request that does
// thousands of bridge calls used to leak thousands of shm pages until end
// of /run, which exceeded /dev/shm capacity under concurrency.
func (d *Dispatcher) dispatch(ctx context.Context, wk *worker.Worker, f *filter.Filter, req *BridgeRequest) *BridgeResponse {
	shmMgr := NewShmManager()
	defer shmMgr.FreeAll()

	fn := req.Fn

	// Probe requests: check whether the target is a module or callable.
	if strings.HasSuffix(fn, ".__probe__") {
		target := strings.TrimSuffix(fn, ".__probe__")
		if !f.Allow(target) && !f.Allow(fn) {
			// Still answer probe for known top-levels even when functions are filtered.
			// Probes for module-level names always succeed (filtering is on calls).
		}

		var pr probeResult
		if cached, ok := d.probeCache.Load(target); ok {
			pr = cached.(probeResult)
		} else {
			msg, err := wk.Probe(target)
			if err != nil {
				return &BridgeResponse{Error: err.Error()}
			}
			pr = probeResult{IsModule: msg.IsModule, IsCallable: msg.IsCallable}
			d.probeCache.Store(target, pr)
		}

		// Encode probe result as a dict-like scalar.
		return &BridgeResponse{Value: &worker.Arg{
			Type: "scalar",
			Value: map[string]interface{}{
				"is_module":   pr.IsModule,
				"is_callable": pr.IsCallable,
			},
		}}
	}

	// Filter check.
	if !f.Allow(fn) {
		return &BridgeResponse{Error: fmt.Sprintf("function %q not allowed by filter profile %q", fn, f.ProfileName)}
	}

	// Resolve ndarray shm handles in args.
	resolvedArgs, err := resolveArgs(req.Args, shmMgr)
	if err != nil {
		return &BridgeResponse{Error: err.Error()}
	}
	resolvedKwargs := make(map[string]worker.Arg)
	for k, v := range req.Kwargs {
		resolved, err := resolveArg(v, shmMgr)
		if err != nil {
			return &BridgeResponse{Error: err.Error()}
		}
		resolvedKwargs[k] = resolved
	}

	resp, err := wk.Call(fn, resolvedArgs, resolvedKwargs)
	if err != nil {
		return &BridgeResponse{Error: err.Error()}
	}
	if resp.Type == "error" {
		return &BridgeResponse{Error: fmt.Sprintf("%s: %s", resp.Error, resp.Message)}
	}

	// If the result is an ndarray, write it to shm and return a handle.
	if resp.Value != nil && resp.Value.Type == worker.KindNDArray {
		encoded, err := encodeNDArrayResult(resp.Value)
		if err != nil {
			return &BridgeResponse{Error: err.Error()}
		}
		return &BridgeResponse{Value: encoded}
	}

	return &BridgeResponse{Value: resp.Value}
}

// resolveArgs resolves a slice of Args, pulling ndarray data from shm.
func resolveArgs(args []worker.Arg, shmMgr *ShmManager) ([]worker.Arg, error) {
	out := make([]worker.Arg, len(args))
	for i, a := range args {
		resolved, err := resolveArg(a, shmMgr)
		if err != nil {
			return nil, err
		}
		out[i] = resolved
	}
	return out, nil
}

// resolveArg converts an ndarray sent from the WASM shim into a worker-bound
// ndarray whose Value carries the path of a /dev/shm file the worker can read.
//
// Two input shapes are accepted:
//   - inline base64 bytes (B64 set) — WASM-originated; cannot touch /dev/shm itself.
//     We unpack into a fresh shm block here.
//   - shm handle (ShmHandle set) — re-passing an array we previously allocated
//     (e.g. when user code passes a worker result back into another bridged call).
func resolveArg(a worker.Arg, shmMgr *ShmManager) (worker.Arg, error) {
	if a.Type != worker.KindNDArray {
		return a, nil
	}

	if a.B64 != "" {
		raw, err := base64.StdEncoding.DecodeString(a.B64)
		if err != nil {
			return a, fmt.Errorf("decode ndarray b64: %w", err)
		}
		h, slice, err := shmMgr.Alloc(len(raw))
		if err != nil {
			return a, fmt.Errorf("alloc shm for ndarray arg: %w", err)
		}
		copy(slice, raw)
		blk, _ := shmMgr.Get(h)
		return worker.Arg{
			Type:      worker.KindNDArray,
			ShmHandle: int(h),
			Shape:     a.Shape,
			DType:     a.DType,
			Value:     shmFilePath(blk.Name),
		}, nil
	}

	blk, ok := shmMgr.Get(uint32(a.ShmHandle))
	if !ok {
		return a, fmt.Errorf("unknown shm handle %d", a.ShmHandle)
	}
	return worker.Arg{
		Type:      worker.KindNDArray,
		ShmHandle: a.ShmHandle,
		Shape:     a.Shape,
		DType:     a.DType,
		Value:     shmFilePath(blk.Name),
	}, nil
}

// encodeNDArrayResult turns a worker-side ndarray result into the wire form
// the WASM shim expects: inline base64 bytes plus shape+dtype.
//
// The worker writes its result to a /dev/shm file and returns the path in
// Value; we read+unlink and inline-encode. We do NOT allocate a Go-side
// shm copy — the WASM shim cannot read /dev/shm regardless, and re-passing
// an ndarray back into a bridge call always re-encodes inline anyway, so
// any Go-side shm copy is dead weight that drives tmpfs pressure under
// concurrency.
func encodeNDArrayResult(arg *worker.Arg) (*worker.Arg, error) {
	shmPath, _ := arg.Value.(string)
	if shmPath == "" {
		// Scalar path fallback if worker returned inline data.
		return arg, nil
	}

	data, err := readShmFile(shmPath)
	if err != nil {
		return nil, fmt.Errorf("read worker shm: %w", err)
	}
	// Worker created the file; Go unlinks once the bytes are read.
	_ = os.Remove(shmPath)

	return &worker.Arg{
		Type:  worker.KindNDArray,
		Shape: arg.Shape,
		DType: arg.DType,
		B64:   base64.StdEncoding.EncodeToString(data),
	}, nil
}

// sendBridgeResponse writes a length-prefixed JSON response to the WASM stdin pipe.
func sendBridgeResponse(w io.Writer, resp *BridgeResponse) {
	data, _ := json.Marshal(resp)
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(data)))
	_, _ = w.Write(hdr)
	_, _ = w.Write(data)
}

// InstantiateWASI configures the WASI host module in the runtime.
// Must be called once before any modules are instantiated.
func InstantiateWASI(ctx context.Context, rt wazero.Runtime) error {
	_, err := wasi_snapshot_preview1.Instantiate(ctx, rt)
	return err
}

// shmFilePath returns the filesystem path for a shm block name.
func shmFilePath(name string) string {
	if _, err := os.Stat("/dev/shm"); err == nil {
		return "/dev/shm/" + name
	}
	return os.TempDir() + "/" + name
}

// readShmFile reads the entire content of a shm file path.
func readShmFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
