# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common commands

- `make python-wasm` — one-time download of `wasm/python.wasm` (~14 MB, CPython-WASI 3.12 from vmware-labs/webassembly-language-runtimes). Required before first build.
- `make build` — `go mod tidy` + `go build -o bin/chrysalis ./cmd/chrysalis`.
- `make run` — build and start the server on port 8080 with all default flag wiring.
- `make test-local` — curl-based smoke tests (numpy.dot, pure Python, safety block). Requires the server to already be running.
- `go test ./...` — there is no test suite yet; verification is via `make test-local` end-to-end.
- Run a single binary subcommand directly: `./bin/chrysalis serve --wasm wasm/python.wasm --filter-dir filters --worker shim/worker.py --shim shim/bootstrap.py --port 8080`.
- Worker dependencies must be installed in the host's `python3`: `pip install numpy scipy` (and pandas if filters reference it).

## Architecture

Chrysalis runs untrusted Python in a WASM sandbox and proxies allowlisted numpy/scipy calls to a warm host-side Python worker. The flow for every `POST /run`:

1. **`internal/handler`** decodes the request, runs **`internal/safety.Check`** (regex pre-check for blocked stdlib imports / blocked builtins; heuristic, not a security boundary), then calls `pool.Run`.
2. **`internal/pool.Pool`** owns a single `wazero.Runtime`, a single `CompiledModule` (compiled once at startup — slow, 10–60s), and a single long-running Python worker. Each request instantiates a fresh WASM module from the shared compiled artifact.
3. **`internal/bridge.Dispatcher.Run`** is the heart of execution. It:
   - Loads the YAML filter profile (`internal/filter`) named by the request.
   - Concatenates `shim/bootstrap.py` + `\n# ---- user code ----\n` + user code, passes it as `python3 -c <fullScript>` args to the WASM module.
   - Wires three pipes: WASM stdin (Go → WASM responses), WASM stdout (WASM → Go requests — repurposed as the bridge channel, NOT user output), and a captured stderr buffer.
   - Spawns a goroutine that reads length-prefixed JSON frames (4-byte big-endian length) from the WASM-stdout pipe, dispatches each, and writes the response back to WASM-stdin.
4. **`shim/bootstrap.py`** runs first inside CPython-WASI. It captures user `print()` into an in-memory `StringIO`, repurposes fd 0/1 as the bridge transport, and installs a PEP 302 meta-path finder so `import numpy` returns a proxy whose attribute access becomes `__shimmy_call__(json) → json` round-trips. At end of script, the captured stdout is sent to Go via a `{"__stdout__": "..."}` sentinel frame.
5. **Bridge dispatch** (`Dispatcher.dispatch`): probe requests (`*.__probe__`) ask the worker whether a name is a module/callable. Real calls go through `filter.Allow(fn)` first; rejection returns an error frame, allowed calls are forwarded to the Python worker via Unix socket.
6. **`internal/worker.Worker`** owns the host-side `python3 shim/worker.py <sockPath>` subprocess, which pre-imports numpy/scipy/pandas and serves length-prefixed JSON over the Unix socket. `Worker.Call`/`Probe`/`Ping`/`Shutdown` all hold a single mutex — the worker is fully serialised, one in-flight request at a time.

### Key invariants and non-obvious points

- **Bridge stdio is not user stdio.** WASM stdout carries JSON frames to Go; user `print()` is captured by `bootstrap.py` and shipped at the end via the `__stdout__` sentinel. The bridge goroutine special-cases that sentinel before attempting to unmarshal a `BridgeRequest`.
- **CPython exits 0 on success, but wazero surfaces this as an error.** `Dispatcher.Run` filters out `exit_code(0)` strings from `execErr` so successful runs don't leak the exit code into the JSON `error` field.
- **Filter eval order** (`internal/filter.Filter.Allow`): exact deny → exact allow → wildcard deny → wildcard allow → default. Wildcard `*` matches across `.` (it is a flat glob, not segment-aware). Profiles live in `filters/*.yaml`; the request's `filter` field is the profile basename.
- **Shared memory** (`internal/bridge/shm*.go`): ndarray payloads cross the WASM/host boundary by file in `/dev/shm` (Linux) or `$TMPDIR` (macOS), tracked by integer handle. `shm_unix.go` is `linux || darwin`; `shm_other.go` is the cross-platform fallback. The Go `Value` field on a `worker.Arg` is overloaded to carry the shm filesystem path when forwarded to the worker.
- **Per-request allocation, shared compilation.** The `wazero.CompiledModule` is reused across requests; only `InstantiateModule` happens per request. Do not re-compile per request — startup will balloon. Likewise the worker is process-wide: do not start a new one per request.
- **Worker is single-threaded.** Concurrent `/run` requests will serialize on `Worker.mu`. If you need real concurrency, that's a worker-pool change, not a per-request fix.
- **The safety pre-check is heuristic.** WASM SFI is the actual boundary. Don't add features that depend on the regex catching everything; add filter rules instead.

### Directory orientation

- `cmd/chrysalis/main.go` — CLI entry, only the `serve` subcommand exists.
- `internal/pool` — module compile + worker bootstrap; the place to wire any new lifecycle resources.
- `internal/bridge` — wazero glue, bridge protocol, shm. Largest blast radius for changes.
- `internal/filter` — YAML profile loader and matcher.
- `internal/worker` — Go side of the worker IPC; `protocol.go` is the wire format shared with `shim/worker.py`.
- `shim/bootstrap.py` — runs *inside* WASM. `shim/worker.py` — runs on the *host*. Easy to mix up; the `import` shim is the WASM-side file.
- `filters/*.yaml` — profile definitions; `autograder` is the default when the request omits `filter`.
- `docs/design.md` — full system design; consult before non-trivial changes to the bridge or worker protocol.
