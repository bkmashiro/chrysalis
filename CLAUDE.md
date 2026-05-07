# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common commands

- `make python-wasm` — one-time download of `wasm/python.wasm` (~14 MB, CPython-WASI 3.12 from vmware-labs/webassembly-language-runtimes). Required before first build.
- `make build` — `go mod tidy` + `go build -o bin/chrysalis ./cmd/chrysalis`.
- `make run` — build and start the server on port 8080 with all default flag wiring.
- `make test-local` — curl-based smoke tests (run against a server already up).
- `make docker-test` (or `bash scripts/docker-test.sh`) — full end-to-end suite: builds the Docker image, starts the server in a container, runs 16 cases (pure Python, numpy/scipy bridge, ndarray round-trip, filter denial, safety pre-check, alt profile, timeout, runtime error). Env vars: `PORT=`, `REBUILD=0` (use cached image), `KEEP_RUNNING=1` (leave container up after).
- `go test ./...` — there is no Go test suite; verification is via `make docker-test` end-to-end.
- Run a single binary subcommand directly: `./bin/chrysalis serve --wasm wasm/python.wasm --filter-dir filters --worker shim/worker.py --shim shim/bootstrap.py --port 8080`.
- Worker dependencies must be installed in the host's `python3`: `pip install numpy scipy` (and pandas if filters reference it). On Debian/Ubuntu, `apt install python3-numpy python3-scipy python3-pandas` works; this is what the Dockerfile does.

## Architecture

Chrysalis runs untrusted Python in a WASM sandbox and proxies allowlisted numpy/scipy calls to a warm host-side Python worker. The flow for every `POST /run`:

1. **`internal/handler`** decodes the request, runs **`internal/safety.Check`** (regex pre-check for blocked stdlib imports / blocked builtins; heuristic, not a security boundary), then calls `pool.Run`.
2. **`internal/pool.Pool`** owns a single `wazero.Runtime`, a single `CompiledModule` (compiled once at startup — slow, 10–60s), and a single long-running Python worker. Each request instantiates a fresh WASM module from the shared compiled artifact.
3. **`internal/bridge.Dispatcher.Run`** is the heart of execution. It:
   - Loads the YAML filter profile (`internal/filter`) named by the request.
   - Concatenates `shim/bootstrap.py` + `\n# ---- user code ----\n` + user code, passes it as `python3 -c <fullScript>` args to the WASM module.
   - Wires three pipes: WASM stdin (Go → WASM responses), WASM stdout (WASM → Go requests — repurposed as the bridge channel, NOT user output), and a captured stderr buffer.
   - Spawns a goroutine that reads length-prefixed JSON frames (4-byte big-endian length) from the WASM-stdout pipe, dispatches each, and writes the response back to WASM-stdin.
4. **`shim/bootstrap.py`** runs first inside CPython-WASI. It captures user `print()` into an in-memory `StringIO`, repurposes fd 0/1 as the bridge transport, and installs a **PEP 451** meta-path finder (`find_spec` + `create_module`/`exec_module` — Python 3.12 dropped the legacy `find_module`/`load_module` hooks; use the modern API). `import numpy` returns a proxy module whose attribute access becomes `__shimmy_call__(json) → json` round-trips. At end of script, the captured stdout is sent to Go via a `{"__stdout__": "..."}` sentinel frame.
5. **Bridge dispatch** (`Dispatcher.dispatch`): probe requests (`*.__probe__`) ask the worker whether a name is a module/callable. Real calls go through `filter.Allow(fn)` first; rejection returns an error frame, allowed calls are forwarded to the Python worker via Unix socket.
6. **`internal/worker.Worker`** owns the host-side `python3 shim/worker.py <sockPath>` subprocess, which pre-imports numpy/scipy/pandas and serves length-prefixed JSON over the Unix socket. `Worker.Call`/`Probe`/`Ping`/`Shutdown` all hold a single mutex — the worker is fully serialised, one in-flight request at a time.

### Key invariants and non-obvious points

- **Bridge stdio is not user stdio.** WASM stdout carries JSON frames to Go; user `print()` is captured by `bootstrap.py` and shipped at the end via the `__stdout__` sentinel. The bridge goroutine special-cases that sentinel before attempting to unmarshal a `BridgeRequest`.
- **CPython exits 0 on success, but wazero surfaces this as an error.** `Dispatcher.Run` filters out `exit_code(0)` strings from `execErr` so successful runs don't leak the exit code into the JSON `error` field.
- **Filter eval order** (`internal/filter.Filter.Allow`): exact deny → exact allow → wildcard deny → wildcard allow → default. Wildcard `*` matches across `.` (it is a flat glob, not segment-aware). Profiles live in `filters/*.yaml`; the request's `filter` field is the profile basename.
- **ndarray transport is asymmetric.** WASM cannot read or write `/dev/shm`, so two encodings coexist on `worker.Arg`:
  - **Worker ↔ Go** uses **binary via shm files** — `shim/worker.py` writes raw `arr.tobytes()` to `/dev/shm/chr-worker-*` and returns the path in `Value`. Go reads it (`encodeNDArrayToShm`) and unlinks. Conversely `resolveArg` writes inbound bytes into a fresh shm block and forwards the path to the worker.
  - **WASM ↔ Go** uses **inline base64** in the `B64` field (`worker.Arg.B64`, JSON `b64`). On WASM-out, `_ser_array` in `bootstrap.py` emits `{type: "ndarray", b64, shape, dtype}`. On WASM-in, the dispatcher fills `B64` so `_deser_ndarray` can rebuild a `_NumpyLike` proxy. The Go side keeps an shm handle alongside, useful when the array is later passed back through the bridge.
- **Shared memory plumbing** (`internal/bridge/shm*.go`): `shm_unix.go` is `linux || darwin` (mmap'd `/dev/shm` if present, else `$TMPDIR`); `shm_other.go` is the cross-platform fallback.
- **Per-request allocation, shared compilation.** The `wazero.CompiledModule` is reused across requests; only `InstantiateModule` happens per request. Do not re-compile per request — startup will balloon. Likewise the worker is process-wide: do not start a new one per request.
- **Worker is single-threaded.** Concurrent `/run` requests will serialize on `Worker.mu`. If you need real concurrency, that's a worker-pool change, not a per-request fix.
- **The safety pre-check is heuristic.** WASM SFI is the actual boundary. Don't add features that depend on the regex catching everything; add filter rules instead.
- **Tight Python loops cannot be preempted by `context.WithTimeout`.** wazero only checks the context at WASI yield points (file/pipe I/O, polling). A pure CPU loop like `while True: i+=1` will run until the WASM module exits naturally; the request goroutine survives until the user code yields. Loops that touch the bridge each iteration (e.g. `while True: np.dot(...)`) preempt fine. Real CPU-fuel cancellation needs wazero's experimental hooks or a fork+rlimit fallback (Phase 2 design item).
- **Float `.0` is dropped in transit.** Worker returns numpy floats as `{"type":"scalar","value":32.0}`; Go's `encoding/json` re-marshals `float64(32.0)` as `"32"`. Cosmetic-only — use `repr()` or formatted printing if exact text matters.

### Directory orientation

- `cmd/chrysalis/main.go` — CLI entry, only the `serve` subcommand exists.
- `internal/pool` — module compile + worker bootstrap; the place to wire any new lifecycle resources.
- `internal/bridge` — wazero glue, bridge protocol, shm. Largest blast radius for changes.
- `internal/filter` — YAML profile loader and matcher.
- `internal/worker` — Go side of the worker IPC; `protocol.go` is the wire format shared with `shim/worker.py`.
- `shim/bootstrap.py` — runs *inside* WASM. `shim/worker.py` — runs on the *host*. Easy to mix up; the `import` shim is the WASM-side file.
- `filters/*.yaml` — profile definitions; `autograder` is the default when the request omits `filter`.
- `Dockerfile` + `scripts/docker-test.sh` — local end-to-end test kit. Single-stage `golang:1.22-bookworm` image with apt-installed `python3-numpy`/`scipy`/`pandas`.
- `docs/design.md` — full system design; consult before non-trivial changes to the bridge or worker protocol.
