# Chrysalis

Sandboxed Python execution engine built on CPython-WASI + wazero.

Runs untrusted Python code with WebAssembly Software Fault Isolation (SFI),
transparently bridging numpy/scipy calls to a warm host-side Python worker.

## Quick Start

### Prerequisites

- Go 1.22+
- Python 3.8+ (for the worker process)
- `numpy`, `scipy` installed in the host Python (`pip install numpy scipy`)

### 1. Download CPython-WASI

```bash
make python-wasm
```

This downloads `wasm/python.wasm` (~14 MB) from the
[webassembly-language-runtimes](https://github.com/vmware-labs/webassembly-language-runtimes) project.

### 2. Build

```bash
make build
```

### 3. Run the server

```bash
make run
```

The server listens on `http://localhost:8080`.

### 4. Test

```bash
make test-local      # quick curl smoke test against a running server
make docker-test     # full end-to-end suite in Docker (16 cases)
```

`make docker-test` builds an image with all deps (numpy/scipy/pandas), starts
the server in a container, and exercises every documented path: pure Python,
numpy/scipy bridge, ndarray round-trip via shm, filter denial, safety
pre-check, alternate profiles, timeout, and runtime errors. Use
`KEEP_RUNNING=1 bash scripts/docker-test.sh` to leave the container up for
debugging.

## API

### `POST /run`

Execute Python code in the sandbox.

**Request body:**

```json
{
  "code": "import numpy as np\nprint(np.dot([1,2,3],[4,5,6]))",
  "filter": "autograder",
  "timeout_sec": 30
}
```

| Field | Type | Default | Description |
|---|---|---|---|
| `code` | string | required | Python source code to execute |
| `filter` | string | `autograder` | Filter profile name (see `filters/`) |
| `timeout_sec` | int | `30` | Execution timeout in seconds |

**Response:**

```json
{
  "status": "ok",
  "stdout": "32.0\n",
  "stderr": "",
  "error": null,
  "timing": {
    "total_ms": 312,
    "exec_ms": 312
  }
}
```

**Status values:** `ok`, `error`, `blocked`

### `GET /health`

Returns `{"status":"ok"}`.

## Filter Profiles

Filter profiles live in `filters/` as YAML files. Three profiles are provided:

| Profile | Description |
|---|---|
| `autograder` | numpy core + linalg + scipy optimize/linalg. No file I/O. |
| `math-only` | Pure math functions only (numpy trig, exp, log, scipy.special). |

### Profile format

```yaml
name: my-profile
description: "..."
default: deny          # or "allow"

allow:
  - numpy.dot
  - numpy.linalg.*   # wildcard: matches any numpy.linalg.X

deny:
  - numpy.load       # explicit deny overrides wildcard allow
```

Evaluation order: exact deny → exact allow → wildcard deny → wildcard allow → default.

## Architecture

```
HTTP /run request
    │
    ▼
Safety pre-check (regex scan for blocked imports/builtins)
    │
    ▼
wazero: Instantiate CPython-WASI module (fresh per request)
    │  stdin  = bridge response pipe (Go → WASM)
    │  stdout = bridge request pipe  (WASM → Go)
    │  stderr = captured
    │
    ├── bootstrap.py injected before user code
    │   • installs PEP 302 meta-path finder for numpy/scipy/pandas
    │   • routes all np.* calls through __shimmy_call__(json) → json
    │
Go bridge goroutine (runs concurrently with WASM):
    │   reads requests from WASM stdout pipe
    │   checks filter (map[string]bool from YAML)
    │   forwards allowed calls to Python worker via Unix socket
    │   writes responses back to WASM stdin pipe
    │
Python worker (long-running subprocess):
    │   pre-loads numpy, scipy, pandas
    │   serves call/probe/ping/shutdown messages
    │   returns results via length-prefixed JSON
```

## Project Structure

```
chrysalis/
  cmd/chrysalis/main.go        CLI entry point
  internal/
    filter/filter.go           YAML filter profiles
    worker/worker.go           Python worker lifecycle
    worker/protocol.go         JSON wire types
    bridge/shm.go              Shared memory allocation
    bridge/dispatcher.go       Execution engine + bridge goroutine
    pool/pool.go               WASM pool (compile-once, instantiate-per-request)
    safety/checker.go          Python code safety pre-check
    handler/handler.go         HTTP handler
  shim/
    bootstrap.py               PEP 302 shim (injected into CPython-WASI)
    worker.py                  Host Python worker process
  filters/
    autograder.yaml
    math-only.yaml
  docs/
    design.md                  Full system design document
```

## Configuration

Server flags:

| Flag | Default | Description |
|---|---|---|
| `--wasm` | `wasm/python.wasm` | Path to CPython-WASI binary |
| `--filter-dir` | `filters` | Directory with YAML profiles |
| `--worker` | `shim/worker.py` | Path to worker.py |
| `--shim` | `shim/bootstrap.py` | Path to bootstrap.py |
| `--sock` | `/tmp/chrysalis_worker.sock` | Unix socket path |
| `--port` | `8080` | HTTP listen port |

## Security

The WASM SFI boundary is the primary security mechanism:

- **Memory**: WASM linear memory is bounds-checked on every access.
- **Syscalls**: WASM cannot execute syscall instructions; all host interaction goes through explicit host functions.
- **CPU**: `context.Context` timeout kills execution via wazero's cancellation checks.
- **Filesystem**: Only WASI pre-opened dirs are visible (none in Phase 1 — user code gets no filesystem).
- **Network**: No network host functions registered.
- **Library calls**: Go filter (from YAML profile) is the sole authority; only allowed functions reach the worker.

The safety pre-check is a fast heuristic filter, not a security boundary.
