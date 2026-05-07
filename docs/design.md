# Chrysalis: System Design Document

## 1. Overview

Chrysalis is a sandboxed Python execution engine designed for AWS Lambda. It enables untrusted Python code to run with strong isolation guarantees in an environment where the host kernel blocks every standard Linux sandboxing mechanism.

The core insight: **WebAssembly Software Fault Isolation (SFI)** provides memory safety, syscall virtualization, and deterministic resource limits without requiring any kernel cooperation. By compiling CPython to WebAssembly (CPython-WASI) and executing it inside wazero (a pure-Go WASM runtime), Chrysalis achieves isolation comparable to a container or microVM — on a kernel that permits neither.

For workloads that require numpy, scipy, or pandas, Chrysalis transparently bridges calls from the sandboxed WASM environment to a warm host-side Python worker process. The bridge uses shared memory for array data and Unix domain sockets for control messages, achieving sub-2ms round-trip times per call.

A secondary **fork path** provides soft isolation via `fork+exec` and `setrlimit` for code that requires the full native Python ecosystem. This path lacks syscall filtering and is suitable only for lower-trust-requirement scenarios.

### Key properties

- **Lambda-compatible**: No ptrace, no seccomp, no namespaces, no eBPF, no Landlock required
- **Strong isolation (WASM path)**: Memory bounds checking, no direct syscalls, CPU limits via context deadline
- **Transparent numpy/scipy**: User code writes `np.dot(a, b)` — the bridge handles everything
- **Low latency**: <1ms pool acquisition, 0.5-2ms bridge RTT, <0.1ms for 1MB array transfer
- **Configurable filtering**: Per-task YAML profiles control which library functions are permitted

---

## 2. Motivation & Constraints

### 2.1 The Lambda sandbox problem

AWS Lambda runs on a Firecracker microVM with a hardened Linux 5.10 kernel. The kernel's seccomp policy and configuration block nearly all mechanisms that a userspace sandbox would use:

| Mechanism | Status on Lambda 5.10 | Failure mode |
|---|---|---|
| `ptrace(PTRACE_TRACEME)` | Blocked | `EPERM` — seccomp filter denies ptrace |
| seccomp-bpf (`prctl(PR_SET_SECCOMP)`) | Blocked | `EPERM` — nested seccomp not allowed |
| User namespaces (`clone(CLONE_NEWUSER)`) | Blocked | `EPERM` — kernel config `CONFIG_USER_NS=n` or seccomp |
| Landlock (`landlock_create_ruleset`) | Blocked | `ENOSYS` — kernel 5.10 predates Landlock (5.13+) |
| eBPF (`bpf()` syscall) | Blocked | `EPERM` — no `CAP_BPF` or `CAP_SYS_ADMIN` |
| DynamoRIO / binary instrumentation | Blocked | `mprotect(PROT_EXEC)` on JIT pages returns `EINVAL` in some code paths; unreliable |
| zpoline (syscall hooking via page 0) | Blocked | `mmap(addr=0)` returns `EINVAL` — `vm.mmap_min_addr` is nonzero |
| Syscall User Dispatch (SUD) | Blocked | `prctl(PR_SET_SYSCALL_USER_DISPATCH)` returns `EINVAL` — kernel 5.10 < 5.11 |

### 2.2 Surviving primitives

The following mechanisms remain available on Lambda and form the foundation of Chrysalis:

| Primitive | Use in Chrysalis |
|---|---|
| `fork` + `exec` | Fork path: spawn isolated child processes |
| `setrlimit` / `prlimit` | Fork path: CPU, address space, process count, file size limits |
| `mmap(PROT_READ\|PROT_WRITE\|PROT_EXEC)` | wazero JIT compilation of WASM modules (limited pages) |
| `userfaultfd` | Future: lazy memory restoration for WASM snapshots |
| Outbound TCP | Lambda-to-Lambda communication, API calls, logging |
| `shm_open` / `mmap` | Zero-copy array transfer between Go host and Python worker |
| Unix domain sockets | Control channel between Go host and Python worker |

### 2.3 Why WASM SFI works

WebAssembly provides isolation through its execution model, not through kernel mechanisms:

1. **Linear memory bounds**: All memory accesses are bounds-checked against a fixed linear memory region. Out-of-bounds access traps immediately.
2. **No direct syscalls**: WASM code cannot execute `syscall` instructions. All interaction with the host goes through explicitly imported host functions.
3. **Control flow integrity**: Indirect calls go through a typed function table. No arbitrary code execution.
4. **Deterministic resource limits**: Memory growth can be capped. CPU time is controlled by the Go runtime's `context.Context` deadline.

wazero enforces all of these at the Go level. It compiles WASM to native code (AOT or JIT), but the compiled code retains all SFI checks. The WASM module has no way to escape its sandbox without exploiting a bug in wazero itself.

---

## 3. Architecture

### 3.1 System diagram

```
┌─────────────────────────────────────────────────────────────────────┐
│                        AWS Lambda Function                          │
│                                                                     │
│  ┌───────────────────────────────────────────────────────────────┐  │
│  │                      Go Handler Process                       │  │
│  │                                                               │  │
│  │  ┌─────────────┐    ┌──────────────────────────────────────┐  │  │
│  │  │   API GW /   │    │         Execution Router             │  │  │
│  │  │   Event In   │───▶│  code_safety_check() ──┐             │  │  │
│  │  └─────────────┘    │                         │             │  │  │
│  │                      │    ┌────────────────────▼──────────┐  │  │  │
│  │                      │    │   WASM Path     │  Fork Path  │  │  │  │
│  │                      │    └───────┬─────────┴──────┬──────┘  │  │  │
│  │                      └────────────┼────────────────┼─────────┘  │  │
│  │                                   │                │            │  │
│  │  ┌────────────────────────────────▼────────┐  ┌────▼────────┐  │  │
│  │  │          WASM Execution Engine          │  │ fork+exec   │  │  │
│  │  │                                         │  │ + setrlimit │  │  │
│  │  │  ┌───────────────────────────────────┐  │  │             │  │  │
│  │  │  │      CompiledModule (cached)      │  │  │  /usr/bin/  │  │  │
│  │  │  │  ┌─────────────────────────────┐  │  │  │  python3    │  │  │
│  │  │  │  │   Instance Pool (chan, 2-4)  │  │  │  └─────────────┘  │  │
│  │  │  │  │  ┌────────┐ ┌────────┐      │  │  │                   │  │
│  │  │  │  │  │ Inst 0 │ │ Inst 1 │ ...  │  │  │                   │  │
│  │  │  │  │  └────┬───┘ └────────┘      │  │  │                   │  │
│  │  │  │  └───────┼─────────────────────┘  │  │                   │  │
│  │  │  └──────────┼────────────────────────┘  │                   │  │
│  │  │             │                            │                   │  │
│  │  │  ┌──────────▼────────────────────────┐  │                   │  │
│  │  │  │    Host Functions (wazero)        │  │                   │  │
│  │  │  │  shimmy_call()                    │  │                   │  │
│  │  │  │  shimmy_shm_alloc()               │  │                   │  │
│  │  │  │  shimmy_shm_free()                │  │                   │  │
│  │  │  │  shimmy_invoke_callback()         │  │                   │  │
│  │  │  │           │                        │  │                   │  │
│  │  │  │  ┌────────▼───────────────────┐   │  │                   │  │
│  │  │  │  │  Filter (map[string]bool)  │   │  │                   │  │
│  │  │  │  │  loaded from YAML profile  │   │  │                   │  │
│  │  │  │  └────────┬───────────────────┘   │  │                   │  │
│  │  │  └───────────┼──────────────────────┘  │                   │  │
│  │  └──────────────┼─────────────────────────┘                   │  │
│  │                 │                                              │  │
│  │       ┌─────────▼───────────┐     ┌──────────────────────┐    │  │
│  │       │   Control Channel   │     │    Data Channel       │    │  │
│  │       │  Unix Domain Socket │     │   POSIX Shared Mem    │    │  │
│  │       │  JSON, len-prefixed │     │   /dev/shm/shimmy_*   │    │  │
│  │       └─────────┬───────────┘     └──────────┬───────────┘    │  │
│  │                 │                             │                │  │
│  │  ┌──────────────▼─────────────────────────────▼────────────┐  │  │
│  │  │              Python Worker Process                      │  │  │
│  │  │  (long-running, preloaded numpy/scipy/pandas)           │  │  │
│  │  │                                                         │  │  │
│  │  │  ┌─────────────┐  ┌──────────────┐  ┌──────────────┐  │  │  │
│  │  │  │  msg loop    │  │  shm reader  │  │  cb_proxy()  │  │  │  │
│  │  │  │  (socket)    │  │  (mmap)      │  │  (callbacks) │  │  │  │
│  │  │  └─────────────┘  └──────────────┘  └──────────────┘  │  │  │
│  │  └─────────────────────────────────────────────────────────┘  │  │
│  └───────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────┘
```

### 3.2 Component descriptions

**Go Handler Process**: The Lambda entry point. Receives invocation events (API Gateway, direct invoke, SQS, etc.), routes to the appropriate execution path, and returns results. Owns the WASM engine and Python worker lifecycle.

**Execution Router**: Decides WASM path vs fork path based on task configuration. Runs the static code safety pre-check (AST analysis) before either path.

**WASM Execution Engine**: Manages the wazero runtime, compiled module cache, and instance pool. Each instance is a fully initialized CPython-WASI environment with the import bridge shim injected.

**Host Functions**: Four wazero-registered functions that the WASM module can call. These are the only way sandboxed code can interact with the outside world. The filter check runs here.

**Filter**: A `map[string]bool` loaded from a YAML profile at task start. The single authority on whether a bridge call is permitted.

**Control Channel**: A Unix domain socket (SOCK_STREAM) carrying length-prefixed JSON messages between the Go host and the Python worker. Used for function call requests, responses, callback invocations, and lifecycle management.

**Data Channel**: POSIX shared memory segments (`/dev/shm/shimmy_*`) used for bulk array transfer. The Go host writes array data from WASM linear memory into shm; the Python worker reads it via `mmap` + `np.frombuffer` (zero-copy on the worker side).

**Python Worker Process**: A long-running Python subprocess that preloads numpy, scipy, and pandas at startup. Receives function call requests over the control channel, executes them using the real native libraries, and returns results. Stateless with respect to filtering — the Go dispatcher handles all access control.

---

## 4. Execution Paths

### 4.1 WASM path (strong isolation)

The WASM path is the primary execution mode. It provides strong isolation through WebAssembly SFI and is suitable for all pure Python workloads, as well as workloads using numpy/scipy/pandas via the bridge.

#### Flow

```
1. Request arrives (user code + task config)
2. Code safety pre-check (AST analysis) → reject or continue
3. Load filter profile from YAML
4. Acquire WasmInstance from pool (or wait for replenishment)
5. Inject user code into instance's virtual filesystem
6. Execute user code with context.Context deadline
7. Collect stdout, stderr, return value
8. Discard instance (do not reuse — user code may have mutated CPython state)
9. Trigger async pool replenishment
10. Return result
```

#### Isolation guarantees

| Property | Mechanism |
|---|---|
| Memory isolation | WASM linear memory: fixed upper bound (e.g., 256 MiB), bounds-checked on every access |
| No syscalls | WASM cannot execute `syscall` instructions; all host interaction via imported functions |
| CPU limit | `context.Context` with deadline; wazero checks cancellation at function call boundaries and loop back-edges |
| Filesystem | WASI `fd_prestat_dir_name` exposes only a single `/sandbox` directory (read-only user code, writable `/tmp`) |
| Network | No network host functions registered; WASM cannot open sockets |
| Library calls | Filtered by Go dispatcher; only whitelisted functions reach the worker |

#### Resource limits

No `setrlimit` calls are needed for the WASM path. WASM provides equivalent or stronger guarantees:

- **Memory**: WASM linear memory has a compile-time maximum (configured at module instantiation). Attempts to `memory.grow` beyond the limit trap.
- **CPU**: `context.WithTimeout` on the Go side. wazero polls for context cancellation. Typical timeout: 30 seconds.
- **Processes**: WASM cannot fork or exec. No child processes possible.
- **Files**: WASI pre-opened directories control filesystem access.

### 4.2 Fork path (soft isolation)

The fork path is a fallback for workloads that require the full native Python ecosystem — packages with C extensions, packages that need filesystem access, or packages not supported by the bridge.

#### Flow

```
1. Request arrives (user code + task config specifying fork path)
2. Code safety pre-check (AST analysis) → reject or continue
3. fork()
4. In child: setrlimit(RLIMIT_CPU, {soft=30, hard=35})
              setrlimit(RLIMIT_AS, {soft=512M, hard=512M})
              setrlimit(RLIMIT_NPROC, {soft=0, hard=0})
              setrlimit(RLIMIT_FSIZE, {soft=10M, hard=10M})
5. exec("/usr/bin/python3", ["-c", user_code])
6. Parent: waitpid() with timeout
7. Collect stdout, stderr, exit code
8. Return result
```

#### Isolation guarantees

| Property | Mechanism | Strength |
|---|---|---|
| CPU limit | `RLIMIT_CPU` (30s soft, 35s hard) | Strong (kernel-enforced SIGKILL) |
| Memory limit | `RLIMIT_AS` (512 MiB) | Strong (kernel-enforced) |
| Process spawning | `RLIMIT_NPROC` = 0 | Moderate (prevents fork bombs, but process already exists) |
| File writes | `RLIMIT_FSIZE` (10 MiB) | Moderate (limits file size, not file count) |
| Syscall filtering | None available | Not enforced |
| Network access | None | Not restricted |
| Filesystem reads | None | Not restricted |

The fork path is explicitly **not** a security boundary. It prevents resource exhaustion but cannot prevent data exfiltration, arbitrary file reads, or outbound network connections. It should only be used when the code source is partially trusted (e.g., instructor-provided code in an autograder) or when the consequences of escape are acceptable.

### 4.3 Path selection

The execution path is determined by the task configuration submitted with each request:

```go
type TaskConfig struct {
    Code          string `json:"code"`
    Path          string `json:"path"`           // "wasm" or "fork"
    FilterProfile string `json:"filter_profile"` // e.g., "autograder", "data-processing"
    TimeoutSec    int    `json:"timeout_sec"`     // default 30
    MaxMemoryMB   int    `json:"max_memory_mb"`   // default 256 (WASM) or 512 (fork)
}
```

Default: WASM path. The fork path must be explicitly requested.

---

## 5. Import Bridge

### 5.1 Problem

User code running inside CPython-WASI needs to call numpy and scipy functions. But numpy and scipy are native C extensions — they cannot be compiled to WASM (they depend on BLAS/LAPACK, Fortran runtime, SIMD intrinsics, etc.). The code must run transparently: `import numpy as np; np.dot(a, b)` should just work.

### 5.2 PEP 302 meta path finder

Python's import system supports **meta path finders** — objects placed on `sys.meta_path` that intercept `import` statements before the default import machinery runs. Chrysalis injects a custom finder before user code executes.

```python
# Injected into CPython-WASI before user code runs
# File: /sandbox/_shimmy_bootstrap.py

import sys
import json

# Bridged modules and their submodules
_BRIDGED_MODULES = {
    "numpy": True,
    "scipy": True,
    "pandas": True,
}

class _ShimmyFinder:
    """PEP 302 meta path finder that intercepts imports of bridged modules."""

    def find_module(self, fullname, path=None):
        top = fullname.split(".")[0]
        if top in _BRIDGED_MODULES:
            return _ShimmyLoader(fullname)
        return None


class _ShimmyLoader:
    """PEP 302 loader that returns proxy modules for bridged packages."""

    def __init__(self, fullname):
        self.fullname = fullname

    def load_module(self, fullname):
        if fullname in sys.modules:
            return sys.modules[fullname]

        mod = _ProxyModule(fullname)
        mod.__loader__ = self
        mod.__package__ = fullname
        mod.__path__ = []  # Mark as package so submodule imports work
        sys.modules[fullname] = mod
        return mod


class _ProxyModule:
    """
    Dynamic proxy that converts attribute access and calls into bridge calls.

    np.dot(a, b) → _bridge_call("numpy.dot", (a, b), {})
    np.linalg.solve(A, b) → _bridge_call("numpy.linalg.solve", (A, b), {})
    np.float64 → _bridge_call("numpy.__getattr__", ("float64",), {})
    """

    def __init__(self, name):
        object.__setattr__(self, "_shimmy_name", name)

    def __getattr__(self, attr):
        if attr.startswith("_"):
            raise AttributeError(attr)

        qualified = f"{self._shimmy_name}.{attr}"

        # Check if this is a submodule (e.g., np.linalg)
        # The host will tell us via the bridge response
        result = _bridge_call(qualified + ".__probe__", (), {})

        if result.get("is_module"):
            sub = _ProxyModule(qualified)
            sys.modules[qualified] = sub
            return sub

        if result.get("is_callable"):
            return _BridgedFunction(qualified)

        # It's an attribute value (e.g., np.pi, np.float64)
        return result.get("value")

    def __repr__(self):
        return f"<bridged module '{self._shimmy_name}'>"


class _BridgedFunction:
    """Callable proxy that forwards invocations to the bridge."""

    def __init__(self, qualified_name):
        self._name = qualified_name

    def __call__(self, *args, **kwargs):
        return _bridge_call(self._name, args, kwargs)

    def __repr__(self):
        return f"<bridged function '{self._name}'>"


# --- Low-level bridge interface ---

_next_cb_id = 0
_callbacks = {}

def _register_callback(fn):
    """Register a Python callable for bidirectional callback from the worker."""
    global _next_cb_id
    cb_id = _next_cb_id
    _next_cb_id += 1
    _callbacks[cb_id] = fn
    return cb_id

def _invoke_callback(cb_id, args_json):
    """Called by the host when the worker invokes a callback."""
    fn = _callbacks[cb_id]
    args = json.loads(args_json)
    result = fn(*args)
    return json.dumps(_serialize(result))

def _bridge_call(func_name, args, kwargs):
    """
    Serialize arguments and invoke the shimmy_call host function.

    Host function signature (WASI):
        shimmy_call(func_ptr, func_len, args_ptr, args_len, kwargs_ptr, kwargs_len)
            -> result_ptr (i32)

    The result is a JSON-encoded response written to WASM linear memory
    by the host. The shim reads it and deserializes.
    """
    serialized_args = json.dumps([_serialize(a) for a in args])
    serialized_kwargs = json.dumps({k: _serialize(v) for k, v in kwargs.items()})

    # _shimmy_raw_call is the actual WASI import — implemented in C
    # in the CPython-WASI build as a thin wrapper around the host function
    result_json = _shimmy_raw_call(func_name, serialized_args, serialized_kwargs)

    result = json.loads(result_json)

    if result.get("error"):
        raise RuntimeError(f"Bridge error in {func_name}: {result['error']}")

    return _deserialize(result.get("value"))


def _serialize(obj):
    """
    Serialize a Python object for bridge transfer.

    - Scalars (int, float, str, bool, None) → {"type": "scalar", "value": ...}
    - Lists/tuples → {"type": "list", "value": [...]}
    - Dicts → {"type": "dict", "value": {...}}
    - ndarray-like (has shape, dtype, tobytes) → written to shm, returns handle
    - Callables → registered as callback, returns cb_id
    """
    if obj is None or isinstance(obj, (int, float, str, bool)):
        return {"type": "scalar", "value": obj}
    elif isinstance(obj, (list, tuple)):
        return {"type": "list", "value": [_serialize(x) for x in obj]}
    elif isinstance(obj, dict):
        return {"type": "dict", "value": {k: _serialize(v) for k, v in obj.items()}}
    elif hasattr(obj, "shape") and hasattr(obj, "dtype") and hasattr(obj, "tobytes"):
        return _serialize_array(obj)
    elif callable(obj):
        cb_id = _register_callback(obj)
        return {"type": "callback", "cb_id": cb_id}
    else:
        return {"type": "scalar", "value": str(obj)}


def _serialize_array(arr):
    """
    Write array data to shared memory via shimmy_shm_alloc host function.
    Returns a handle referencing the shm block.
    """
    data = arr.tobytes()
    shape = list(arr.shape)
    dtype_str = str(arr.dtype)

    # shimmy_shm_alloc writes data from WASM linear memory → shm
    # Returns an opaque handle (integer)
    handle = _shimmy_shm_alloc(data, shape, dtype_str)

    return {
        "type": "ndarray",
        "shm_handle": handle,
        "shape": shape,
        "dtype": dtype_str,
    }


def _deserialize(obj):
    """Deserialize a bridge response back to a Python object."""
    if obj is None:
        return None
    t = obj.get("type")
    if t == "scalar":
        return obj["value"]
    elif t == "list":
        return [_deserialize(x) for x in obj["value"]]
    elif t == "dict":
        return {k: _deserialize(v) for k, v in obj["value"].items()}
    elif t == "ndarray":
        # Read array data from shm
        return _shimmy_shm_read(obj["shm_handle"], obj["shape"], obj["dtype"])
    else:
        return obj.get("value")


# Install the finder
sys.meta_path.insert(0, _ShimmyFinder())
```

### 5.3 Attribute access flow

The proxy module uses a **probe** mechanism to distinguish between submodules, callable functions, and plain attributes:

```
User code: np.linalg.solve(A, b)

Step 1: np.linalg
  → _ProxyModule("numpy").__getattr__("linalg")
  → _bridge_call("numpy.linalg.__probe__", (), {})
  → Host checks: is "numpy.linalg" a module? Yes.
  → Returns {"is_module": True}
  → Creates _ProxyModule("numpy.linalg"), caches in sys.modules

Step 2: np.linalg.solve
  → _ProxyModule("numpy.linalg").__getattr__("solve")
  → _bridge_call("numpy.linalg.solve.__probe__", (), {})
  → Host checks: is "numpy.linalg.solve" callable? Yes.
  → Returns {"is_callable": True}
  → Returns _BridgedFunction("numpy.linalg.solve")

Step 3: np.linalg.solve(A, b)
  → _BridgedFunction("numpy.linalg.solve").__call__(A, b)
  → A serialized to shm (via _serialize_array)
  → b serialized to shm (via _serialize_array)
  → _bridge_call("numpy.linalg.solve", (A_handle, b_handle), {})
  → Go dispatcher checks filter → allowed
  → Go dispatcher forwards to Python worker
  → Worker reconstructs arrays from shm, calls real numpy.linalg.solve
  → Result written to shm, handle returned
  → WASM shim reads result from shm, returns as ndarray-like object
```

---

## 6. Bridge Protocol

### 6.1 Host functions

Four host functions are registered with wazero. These are the only way WASM code can interact with the outside world beyond basic stdout/stderr.

```go
// Registered as WASI host function imports in the "shimmy" namespace

// shimmy_call: invoke a bridged library function
// Params:
//   func_ptr, func_len   - pointer and length of function name string in WASM memory
//   args_ptr, args_len   - pointer and length of JSON-encoded arguments
//   kwargs_ptr, kwargs_len - pointer and length of JSON-encoded keyword arguments
// Returns:
//   result_ptr (i32)     - pointer to JSON-encoded result in WASM memory
//                          (allocated by the host using the WASM allocator export)
func shimmy_call(ctx context.Context, mod api.Module,
    funcPtr, funcLen, argsPtr, argsLen, kwargsPtr, kwargsLen uint32) uint32

// shimmy_shm_alloc: copy array data from WASM linear memory to shared memory
// Params:
//   data_ptr, data_len   - raw array bytes in WASM memory
//   meta_ptr, meta_len   - JSON-encoded metadata (shape, dtype) in WASM memory
// Returns:
//   handle (i32)         - opaque shm block handle (index into handle table)
func shimmy_shm_alloc(ctx context.Context, mod api.Module,
    dataPtr, dataLen, metaPtr, metaLen uint32) uint32

// shimmy_shm_free: release a shared memory block
// Params:
//   handle (i32)         - handle from shimmy_shm_alloc
func shimmy_shm_free(ctx context.Context, mod api.Module, handle uint32)

// shimmy_invoke_callback: invoke a WASM-side callback (used by worker via Go)
// Params:
//   cb_id (i32)          - callback ID registered by the WASM shim
//   args_ptr, args_len   - JSON-encoded callback arguments in WASM memory
// Returns:
//   result_ptr (i32)     - pointer to JSON-encoded result in WASM memory
func shimmy_invoke_callback(ctx context.Context, mod api.Module,
    cbID, argsPtr, argsLen uint32) uint32
```

The WASM module exports a memory allocator (`__shimmy_alloc(size) → ptr`) that the host uses to write response data into WASM linear memory. It also exports `__shimmy_invoke_cb(cb_id, args_ptr, args_len) → result_ptr` for bidirectional callbacks.

### 6.2 Control channel

The control channel is a Unix domain socket (SOCK_STREAM) connecting the Go host to the Python worker. Messages are length-prefixed JSON.

#### Wire format

```
┌──────────────────┬────────────────────────────────┐
│  4 bytes (BE)    │  N bytes (UTF-8 JSON)          │
│  message length  │  message body                  │
└──────────────────┴────────────────────────────────┘
```

The 4-byte length prefix is a big-endian unsigned 32-bit integer encoding the length of the JSON body in bytes. Maximum message size: 16 MiB (enforced by both sides).

#### Message types

**Request (Go → Worker)**:

```json
{
    "id": "req-001",
    "type": "call",
    "func": "numpy.dot",
    "args": [
        {"type": "ndarray", "shm_handle": 0, "shape": [100, 100], "dtype": "float64"},
        {"type": "ndarray", "shm_handle": 1, "shape": [100, 100], "dtype": "float64"}
    ],
    "kwargs": {}
}
```

**Response (Worker → Go)**:

```json
{
    "id": "req-001",
    "type": "result",
    "value": {
        "type": "ndarray",
        "shm_handle": 2,
        "shape": [100, 100],
        "dtype": "float64"
    }
}
```

**Error response (Worker → Go)**:

```json
{
    "id": "req-001",
    "type": "error",
    "error": "ValueError",
    "message": "shapes (100,100) and (50,50) not aligned"
}
```

**Callback invocation (Worker → Go)**:

```json
{
    "id": "cb-001",
    "type": "callback",
    "cb_id": 0,
    "args": [1.5, 2.3, 0.7]
}
```

**Callback result (Go → Worker)**:

```json
{
    "id": "cb-001",
    "type": "callback_result",
    "value": {"type": "scalar", "value": 3.14}
}
```

**Probe request (Go → Worker)**:

```json
{
    "id": "req-002",
    "type": "probe",
    "target": "numpy.linalg"
}
```

**Probe response (Worker → Go)**:

```json
{
    "id": "req-002",
    "type": "probe_result",
    "is_module": true,
    "is_callable": false
}
```

**Lifecycle messages**:

```json
{"type": "ping"}
{"type": "pong"}
{"type": "shutdown"}
{"type": "shutdown_ack"}
```

### 6.3 Data channel (shared memory)

Array data is transferred via POSIX shared memory (`shm_open`). This avoids serializing large arrays through the Unix socket.

#### shm block layout

Each shared memory segment has a fixed 64-byte header followed by raw array data:

```
Offset  Size    Field           Description
──────  ──────  ──────────────  ────────────────────────────────────────
0       4       magic           "NPAY" (0x4E504159) — numpy array magic
4       4       version         Protocol version (currently 1)
8       4       ndim            Number of dimensions (max 32)
12      4       dtype_code      Enumerated dtype (0=float32, 1=float64,
                                2=int32, 3=int64, 4=uint8, ...)
16      4       itemsize        Size of one element in bytes
20      4       flags           Bit flags: 0x01=C_CONTIGUOUS,
                                0x02=F_CONTIGUOUS
24      32      shape           Up to 8 × uint32 dimension sizes
                                (ndim entries used, rest zero-padded)
56      4       data_bytes      Total size of raw data in bytes
60      4       reserved        Reserved for future use (zero)
──────  ──────  ──────────────  ────────────────────────────────────────
64      N       data            Raw array data (C-contiguous by default)
```

All multi-byte fields are little-endian (matching the WASM linear memory byte order and typical x86 Lambda environments).

#### shm naming and lifecycle

```
/dev/shm/shimmy_{instance_id}_{seq}
```

- `instance_id`: unique per WasmInstance (e.g., monotonic counter)
- `seq`: monotonic per instance, incremented for each allocation

The Go host maintains a handle table per WasmInstance:

```go
type ShmBlock struct {
    Name     string   // e.g., "shimmy_3_17"
    Fd       int      // file descriptor from shm_open
    Addr     uintptr  // mmap'd address in Go process
    Size     int      // total size (header + data)
    RefCount int32    // atomic; freed when reaches 0
}

type ShmManager struct {
    mu     sync.Mutex
    blocks map[uint32]*ShmBlock  // handle → block
    nextH  uint32
}
```

#### The two copies

WASM's SFI model makes it impossible to directly expose WASM linear memory to external processes (the memory is a Go `[]byte` managed by wazero). Two memory copies are therefore unavoidable:

```
Copy 1: WASM linear memory → shared memory (in shimmy_shm_alloc)
         Go reads from mod.Memory().Read(dataPtr, dataLen)
         Go writes to mmap'd shm region

Copy 2: shared memory → WASM linear memory (in shimmy_call response path)
         Go reads from mmap'd shm region
         Go writes to mod.Memory().Write(resultPtr, data)
```

The Python worker side incurs **zero copies**: it mmaps the shm segment and creates a numpy array via `np.frombuffer(mmap_view, dtype=dtype).reshape(shape)`, which shares the underlying buffer.

For a 1 MB array, each copy takes <0.1ms. For a 100 MB array, each copy takes ~10ms. This is acceptable for the target use case (student assignments, data processing pipelines) where arrays rarely exceed 10 MB.

#### Serialization strategy

| Data type | Serialization | Channel |
|---|---|---|
| Scalars (int, float, str, bool, None) | JSON | Control (socket) |
| Small lists, dicts, tuples | JSON | Control (socket) |
| numpy arrays | Raw bytes + header | Data (shm) |
| Callback references | cb_id integer | Control (socket) |
| Exceptions | Error type + message | Control (socket) |

The threshold for "small" is implicit: anything with a `shape`, `dtype`, and `tobytes` method goes through shm. Everything else goes through JSON on the control channel.

---

## 7. Filter System

### 7.1 Architecture

The filter is a single-layer check in the Go dispatcher. It is the **only** filter in the system — there is no secondary check in the WASM shim or the Python worker. This simplicity is deliberate: one authoritative check point, one configuration format, one place to audit.

```go
type Filter struct {
    Allowed map[string]bool  // "numpy.dot" → true, "os.system" → false
    Default bool             // action for unlisted functions (default: deny)
}

func (f *Filter) Check(funcName string) bool {
    if allowed, exists := f.Allowed[funcName]; exists {
        return allowed
    }
    return f.Default
}
```

The filter runs inside `shimmy_call` before any message is sent to the Python worker:

```go
func shimmy_call(ctx context.Context, mod api.Module, ...) uint32 {
    funcName := readString(mod, funcPtr, funcLen)

    // Single filter check — the only authority
    if !instance.filter.Check(funcName) {
        return writeError(mod, fmt.Sprintf("function %q not allowed by filter profile %q",
            funcName, instance.filterProfile))
    }

    // Forward to Python worker
    response := instance.worker.Call(ctx, funcName, args, kwargs)
    return writeResponse(mod, response)
}
```

### 7.2 YAML profile format

Filter profiles are stored as YAML files in the `filters/` directory:

```yaml
# filters/autograder.yaml
# Profile for student code autograding
# Only math and array operations allowed

name: autograder
description: "Restricted profile for student code evaluation"
default: deny

allow:
  # numpy core
  - numpy.dot
  - numpy.matmul
  - numpy.array
  - numpy.zeros
  - numpy.ones
  - numpy.eye
  - numpy.arange
  - numpy.linspace
  - numpy.reshape
  - numpy.transpose
  - numpy.concatenate
  - numpy.stack
  - numpy.split

  # numpy math
  - numpy.sum
  - numpy.mean
  - numpy.std
  - numpy.var
  - numpy.min
  - numpy.max
  - numpy.abs
  - numpy.sqrt
  - numpy.exp
  - numpy.log
  - numpy.sin
  - numpy.cos
  - numpy.tan

  # numpy linalg
  - numpy.linalg.solve
  - numpy.linalg.inv
  - numpy.linalg.det
  - numpy.linalg.eig
  - numpy.linalg.eigvals
  - numpy.linalg.norm
  - numpy.linalg.svd

  # numpy random (seeded only — stateless)
  - numpy.random.seed
  - numpy.random.rand
  - numpy.random.randn
  - numpy.random.randint

  # scipy optimize
  - scipy.optimize.minimize
  - scipy.optimize.root

  # scipy linalg
  - scipy.linalg.solve
  - scipy.linalg.lu
  - scipy.linalg.qr

  # Attribute/type probes (always allowed)
  - "*.___probe__"

deny:
  # Explicitly deny dangerous operations
  - numpy.load    # reads files
  - numpy.save    # writes files
  - numpy.loadtxt # reads files
  - numpy.savetxt # writes files
  - scipy.io.*    # all I/O
  - pandas.read_* # all file readers
```

```yaml
# filters/data-processing.yaml
# Broader profile for data processing workloads

name: data-processing
description: "Full numpy/scipy/pandas access except I/O"
default: deny

allow:
  - numpy.*
  - scipy.*
  - pandas.*

deny:
  # File I/O
  - numpy.load
  - numpy.save
  - numpy.loadtxt
  - numpy.savetxt
  - numpy.genfromtxt
  - scipy.io.*
  - pandas.read_csv
  - pandas.read_excel
  - pandas.read_json
  - pandas.read_parquet
  - pandas.read_sql
  - pandas.DataFrame.to_csv
  - pandas.DataFrame.to_excel
  - pandas.DataFrame.to_json
  - pandas.DataFrame.to_parquet
  - pandas.DataFrame.to_sql

  # Network
  - pandas.read_html
  - pandas.read_fwf
```

```yaml
# filters/math-only.yaml
# Minimal profile: pure math, no data structures

name: math-only
description: "Mathematical functions only"
default: deny

allow:
  - numpy.sin
  - numpy.cos
  - numpy.tan
  - numpy.exp
  - numpy.log
  - numpy.sqrt
  - numpy.abs
  - numpy.pi
  - numpy.e
  - scipy.special.*
```

### 7.3 Wildcard matching

The filter supports glob-style wildcards:

```go
func (f *Filter) Check(funcName string) bool {
    // 1. Exact match (O(1) hashmap lookup)
    if allowed, exists := f.Allowed[funcName]; exists {
        return allowed
    }

    // 2. Check deny wildcards first (deny takes precedence)
    for pattern, denied := range f.Denied {
        if denied && matchWildcard(pattern, funcName) {
            return false
        }
    }

    // 3. Check allow wildcards
    for pattern, allowed := range f.Allowed {
        if allowed && matchWildcard(pattern, funcName) {
            return true
        }
    }

    return f.Default
}
```

Evaluation order: exact deny → exact allow → wildcard deny → wildcard allow → default. Deny always takes precedence over allow at the same specificity level.

### 7.4 Filter loading and per-instance binding

```go
func LoadFilter(profileName string) (*Filter, error) {
    path := filepath.Join("filters", profileName+".yaml")
    data, err := os.ReadFile(path)
    if err != nil {
        return nil, fmt.Errorf("filter profile %q not found: %w", profileName, err)
    }

    var cfg FilterConfig
    if err := yaml.Unmarshal(data, &cfg); err != nil {
        return nil, fmt.Errorf("invalid filter profile %q: %w", profileName, err)
    }

    filter := &Filter{
        Allowed: make(map[string]bool),
        Default: cfg.Default == "allow",
    }

    for _, pattern := range cfg.Allow {
        filter.Allowed[pattern] = true
    }
    for _, pattern := range cfg.Deny {
        filter.Allowed[pattern] = false
    }

    return filter, nil
}
```

Filters are loaded once per task and bound to the WasmInstance:

```go
type WasmInstance struct {
    mod          api.Module
    filter       *Filter
    filterProfile string
    shmManager   *ShmManager
    worker       *WorkerConn
    // ...
}
```

### 7.5 Multiplexing filters over a shared worker

Multiple WasmInstances with different filter profiles can share a single Python worker. The worker executes whatever function it receives — it has no concept of filtering. All access control is enforced at the Go dispatcher level before the request reaches the worker.

```
Instance A (autograder.yaml) ──┐
                                ├──▶ Go Dispatcher (filter check) ──▶ Worker
Instance B (data-processing.yaml) ──┘
```

This design means the worker never needs to be restarted when the filter profile changes. It also means the worker cannot be tricked into executing filtered functions by a malicious WASM instance — the Go dispatcher is the single choke point.

---

## 8. Bidirectional Callbacks

### 8.1 Problem

Some library functions require user-provided callables. The canonical example:

```python
from scipy.optimize import minimize

def objective(x):
    return (x[0] - 1)**2 + (x[1] - 2)**2

result = minimize(objective, x0=[0, 0])
```

Here, `minimize` must call `objective` repeatedly. But `objective` is user code running in the WASM sandbox, while `minimize` runs in the Python worker. The worker needs to call back into the WASM sandbox.

### 8.2 Solution

The WASM shim registers callables and passes opaque callback IDs through the bridge. The worker wraps these IDs into callable proxies that invoke the WASM-side function via the Go dispatcher.

### 8.3 Flow diagram

```
WASM (CPython-WASI)              Go Dispatcher              Python Worker
─────────────────              ──────────────              ─────────────
      │                              │                          │
  1.  │ cb_id = register(objective)  │                          │
      │                              │                          │
  2.  │ ── shimmy_call ──────────▶   │                          │
      │    "scipy.optimize.minimize" │                          │
      │    args: [{cb_id: 0}, ...]   │                          │
      │                              │  3. ── call ──────────▶  │
      │                              │     "scipy.optimize.     │
      │                              │      minimize"           │
      │                              │     args: [cb_proxy, ...]│
      │                              │                          │
      │                              │                     4.   │ scipy calls
      │                              │                          │ cb_proxy(x)
      │                              │                          │
      │                              │  5. ◀── callback ─────  │
      │                              │     cb_id: 0             │
      │                              │     args: [x]            │
      │                              │                          │
  6.  │ ◀── invoke_callback ──────   │                          │
      │     cb_id: 0, args: [x]     │                          │
      │                              │                          │
  7.  │ objective(x) → result       │                          │
      │                              │                          │
  8.  │ ── callback_result ───────▶  │                          │
      │     value: result            │                          │
      │                              │  9. ── cb_result ──────▶ │
      │                              │     value: result        │
      │                              │                          │
      │                              │                    10.   │ cb_proxy returns
      │                              │                          │ result to scipy
      │                              │                          │
      │                     ... (scipy iterates, steps 4-10 repeat) ...
      │                              │                          │
      │                              │ 11. ◀── result ────────  │
      │                              │     final result         │
      │                              │                          │
 12.  │ ◀── shimmy_call result ────  │                          │
      │     final result             │                          │
```

### 8.4 Worker callback proxy

```python
# In the Python Worker process

class CallbackProxy:
    """Wraps a WASM-side callback as a Python callable for use by scipy/etc."""

    def __init__(self, cb_id, conn):
        self.cb_id = cb_id
        self.conn = conn  # connection back to Go dispatcher

    def __call__(self, *args):
        # Convert numpy arrays in args to shm handles
        serialized = [serialize_arg(a) for a in args]

        # Send callback invocation to Go dispatcher
        self.conn.send({
            "id": f"cb-{self.cb_id}-{next_seq()}",
            "type": "callback",
            "cb_id": self.cb_id,
            "args": serialized,
        })

        # Block waiting for callback result
        response = self.conn.recv_callback_result()

        return deserialize_result(response["value"])
```

### 8.5 Depth limit

Without a depth limit, malicious code could create deeply recursive callbacks:

```python
def evil(x):
    from scipy.optimize import minimize
    return minimize(evil, x).fun  # recursive callback → stack overflow
```

The Go dispatcher enforces a maximum callback depth:

```go
const MaxCallbackDepth = 8

func (d *Dispatcher) InvokeCallback(ctx context.Context, inst *WasmInstance,
    cbID uint32, args []byte) ([]byte, error) {

    depth := ctx.Value(callbackDepthKey{}).(int)
    if depth >= MaxCallbackDepth {
        return nil, fmt.Errorf("callback depth limit exceeded (%d)", MaxCallbackDepth)
    }

    childCtx := context.WithValue(ctx, callbackDepthKey{}, depth+1)
    return inst.InvokeExportedCallback(childCtx, cbID, args)
}
```

The depth limit of 8 is chosen to accommodate legitimate use cases (scipy.optimize can nest 2-3 deep in some algorithms) while preventing stack exhaustion. Each callback level consumes approximately 8 KB of Go stack and 16 KB of WASM stack.

### 8.6 Callback cleanup

Callbacks are scoped to a single `shimmy_call` invocation. When the top-level bridge call completes:

1. The Go dispatcher discards the callback table for that invocation
2. Any pending callback requests from the worker are rejected with an error
3. The WASM shim clears its `_callbacks` dict

This prevents callback ID reuse attacks across invocations.

---

## 9. Warm Multiplexing

### 9.1 Cold start problem

Lambda cold starts are expensive. CPython-WASI initialization compounds this:

| Phase | Time | Happens when |
|---|---|---|
| Lambda container startup | 500ms-2s | Cold start only |
| wazero WASM compilation (AOT) | 1-3s | Cold start only |
| Python worker + numpy import | 1-2s | Cold start only |
| CPython-WASI initialization per instance | 60-250ms | Every request (naive) |

Without warm multiplexing, every request pays at least 60-250ms for CPython-WASI init, even on a warm container. With a pool, hot requests pay <1ms.

### 9.2 CompiledModule

wazero supports ahead-of-time compilation of WASM modules. The compiled representation is reusable across instances.

```go
type Engine struct {
    runtime    wazero.Runtime
    compiled   wazero.CompiledModule  // compiled once, reused forever
    pool       chan *WasmInstance
    poolSize   int
    worker     *WorkerProcess
    shmMgr     *ShmManager

    mu         sync.Mutex
    nextInstID uint32
}

func NewEngine(ctx context.Context, wasmBytes []byte, poolSize int) (*Engine, error) {
    rt := wazero.NewRuntime(ctx)

    // Compile once — this is the expensive step (1-3s)
    compiled, err := rt.CompileModule(ctx, wasmBytes)
    if err != nil {
        return nil, fmt.Errorf("WASM compile: %w", err)
    }

    e := &Engine{
        runtime:  rt,
        compiled: compiled,
        pool:     make(chan *WasmInstance, poolSize),
        poolSize: poolSize,
    }

    // Pre-fill pool
    for i := 0; i < poolSize; i++ {
        inst, err := e.newInstance(ctx)
        if err != nil {
            return nil, fmt.Errorf("pre-fill instance %d: %w", i, err)
        }
        e.pool <- inst
    }

    return e, nil
}
```

### 9.3 Instance pool

The pool is a buffered Go channel holding pre-initialized WasmInstances. Each instance has CPython-WASI fully booted (site.py loaded, shimmy shim injected, `sys.meta_path` configured).

```go
func (e *Engine) Acquire(ctx context.Context) (*WasmInstance, error) {
    select {
    case inst := <-e.pool:
        return inst, nil
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}

func (e *Engine) Release(inst *WasmInstance) {
    // Do NOT return to pool — instance state is tainted by user code.
    // Close and replenish asynchronously.
    go func() {
        inst.Close()
        newInst, err := e.newInstance(context.Background())
        if err != nil {
            log.Printf("pool replenish failed: %v", err)
            return
        }
        e.pool <- newInst
    }()
}
```

Instances are **never reused** after executing user code. User code may have mutated CPython globals, imported modules, or corrupted interpreter state. The cost of a fresh instance (~60-250ms) is paid asynchronously while the current request is being served.

### 9.4 Instance lifecycle

```
┌──────────┐     ┌──────────────┐     ┌──────────────┐     ┌──────────┐
│  Compile │────▶│ Instantiate  │────▶│   In Pool    │────▶│ Acquired │
│  Module  │     │ + Init CPy   │     │  (waiting)   │     │ (in use) │
└──────────┘     └──────────────┘     └──────────────┘     └────┬─────┘
     │                                       ▲                   │
     │                                       │              user code
     │                                       │              executes
     │                                       │                   │
     │                                       │              ┌────▼─────┐
     │                                  replenish           │ Discard  │
     │                                  (async)             │ (close)  │
     │                                       │              └────┬─────┘
     │                                       │                   │
     │                                  ┌────┴─────┐             │
     │                                  │  New     │◀────────────┘
     │                                  │ Instance │
     │                                  └──────────┘
```

### 9.5 Python worker lifecycle

The Python worker is a single long-running subprocess. It preloads heavy libraries at startup and serves all requests.

```go
type WorkerProcess struct {
    cmd     *exec.Cmd
    conn    net.Conn       // Unix domain socket
    mu      sync.Mutex     // serializes requests (single worker)
    healthy bool
}

func StartWorker(ctx context.Context, socketPath string) (*WorkerProcess, error) {
    // Create Unix domain socket
    listener, err := net.Listen("unix", socketPath)
    if err != nil {
        return nil, err
    }

    cmd := exec.CommandContext(ctx, "python3", "-u", "worker.py", socketPath)
    cmd.Stdout = os.Stderr  // worker logs go to stderr
    cmd.Stderr = os.Stderr

    if err := cmd.Start(); err != nil {
        return nil, err
    }

    // Wait for worker to connect
    conn, err := listener.Accept()
    if err != nil {
        return nil, err
    }

    // Wait for "ready" message (worker has finished importing numpy/scipy)
    msg, err := readMessage(conn)
    if err != nil || msg.Type != "ready" {
        return nil, fmt.Errorf("worker failed to initialize: %v", err)
    }

    return &WorkerProcess{cmd: cmd, conn: conn, healthy: true}, nil
}
```

Worker Python code (simplified):

```python
#!/usr/bin/env python3
"""Chrysalis Python Worker — preloads heavy libraries and serves bridge calls."""

import socket
import struct
import json
import sys
import os
import importlib
import mmap

# --- Preload heavy libraries at startup ---
import numpy as np
import scipy
import scipy.optimize
import scipy.linalg
import scipy.special

try:
    import pandas as pd
except ImportError:
    pd = None

# --- Connect to Go host ---
sock_path = sys.argv[1]
sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.connect(sock_path)

def send_msg(msg):
    data = json.dumps(msg).encode("utf-8")
    sock.sendall(struct.pack(">I", len(data)) + data)

def recv_msg():
    header = sock.recv(4)
    if len(header) < 4:
        raise ConnectionError("connection closed")
    length = struct.unpack(">I", header)[0]
    chunks = []
    remaining = length
    while remaining > 0:
        chunk = sock.recv(min(remaining, 65536))
        if not chunk:
            raise ConnectionError("connection closed")
        chunks.append(chunk)
        remaining -= len(chunk)
    return json.loads(b"".join(chunks))

# Signal ready
send_msg({"type": "ready"})

# --- Main loop ---
while True:
    msg = recv_msg()

    if msg["type"] == "shutdown":
        send_msg({"type": "shutdown_ack"})
        break

    elif msg["type"] == "ping":
        send_msg({"type": "pong"})

    elif msg["type"] == "probe":
        target = msg["target"]
        parts = target.split(".")
        try:
            obj = importlib.import_module(parts[0])
            for attr in parts[1:]:
                obj = getattr(obj, attr)
            send_msg({
                "id": msg["id"],
                "type": "probe_result",
                "is_module": hasattr(obj, "__path__") or hasattr(obj, "__file__"),
                "is_callable": callable(obj),
            })
        except (ImportError, AttributeError):
            send_msg({
                "id": msg["id"],
                "type": "probe_result",
                "is_module": False,
                "is_callable": False,
            })

    elif msg["type"] == "call":
        try:
            result = execute_call(msg)
            send_msg({"id": msg["id"], "type": "result", "value": result})
        except Exception as e:
            send_msg({
                "id": msg["id"],
                "type": "error",
                "error": type(e).__name__,
                "message": str(e),
            })

    elif msg["type"] == "callback_result":
        # Handled inline during callback flow
        pass
```

### 9.6 Worker pool (future)

For concurrent request handling, the worker can be extended to a pool:

```go
type WorkerPool struct {
    workers chan *WorkerProcess
    size    int
}
```

Currently, requests are serialized through a single worker. This is acceptable because:
1. Lambda typically handles one invocation at a time (reserved concurrency = 1)
2. The worker is CPU-bound during numpy calls — parallelism would contend for the same cores
3. Bridge call RTT is 0.5-2ms — serialization overhead is negligible for typical workloads

### 9.7 Memory snapshot (future optimization)

After CPython-WASI initializes (60-250ms), the WASM linear memory contains a fully booted Python interpreter. This memory state can be snapshotted and restored:

```go
// Snapshot: save linear memory after init
func (e *Engine) Snapshot(inst *WasmInstance) ([]byte, error) {
    mem := inst.mod.Memory()
    size := mem.Size()
    snapshot := make([]byte, size)
    copy(snapshot, mem.Read(0, size))
    return snapshot, nil
}

// Restore: overwrite linear memory with snapshot
func (e *Engine) RestoreFromSnapshot(ctx context.Context, snapshot []byte) (*WasmInstance, error) {
    inst, err := e.compiled.Instantiate(ctx)  // fast: no CPython init
    if err != nil {
        return nil, err
    }
    mem := inst.Memory()
    mem.Write(0, snapshot)
    // Reset mutable interpreter state (GIL, fd table, etc.) via exported function
    inst.ExportedFunction("__shimmy_post_restore").Call(ctx)
    return &WasmInstance{mod: inst}, nil
}
```

Expected improvement: **~10-20ms** restore from snapshot vs **60-250ms** full CPython init. This optimization requires:

1. Careful handling of WASM global variables (they must be snapshot'd too)
2. A `__shimmy_post_restore` function that reinitializes runtime state (signal handlers, fd table, random seed)
3. Testing for CPython internal invariants that may break across snapshot/restore

wazero does not currently expose a snapshot API, so this requires either upstream contribution or a custom fork. An alternative is `userfaultfd`-based lazy page restoration, which Lambda does support.

---

## 10. Smart Filter (Future Work)

The current filter system uses manually maintained YAML whitelists. Three approaches can automate whitelist generation. These are designed but not implemented in Phase 1.

### 10.1 Offline strace profiling

Run each numpy/scipy function under strace on a development machine, recording syscall profiles:

```bash
# Profile numpy.dot
strace -f -e trace=file,network,process -o /tmp/numpy_dot.trace \
    python3 -c "import numpy as np; np.dot(np.ones((100,100)), np.ones((100,100)))"

# Parse trace for dangerous syscalls
grep -E "^(open|connect|bind|listen|accept|fork|clone|exec)" /tmp/numpy_dot.trace
```

#### Classification rules

| Syscall category | Classification | Action |
|---|---|---|
| `read`, `write`, `mmap`, `brk`, `mprotect` | Computation | Allow |
| `open("/usr/lib/...")`, `open("/etc/ld.so.cache")` | Library loading | Allow |
| `open("/tmp/...")`, `open` with user-controlled path | File I/O | Deny |
| `connect`, `bind`, `listen`, `accept` | Network | Deny |
| `fork`, `clone`, `execve` | Process creation | Deny |

#### Auto-generation pipeline

```
1. For each function F in numpy/scipy public API:
   a. Run F under strace with representative inputs
   b. Parse syscall trace
   c. If trace contains ONLY computation + library-loading syscalls: ALLOW
   d. If trace contains file/network/process syscalls: DENY
   e. If trace is ambiguous (e.g., tempfile for LAPACK workspace): REVIEW

2. Generate YAML whitelist from results
3. Human reviews DENY and REVIEW entries
4. Ship as default profile
```

#### Limitations

- Strace profiles are input-dependent: some functions only touch files for certain arguments
- Must be re-run for each numpy/scipy version
- Cannot run on Lambda (no ptrace) — development machine only
- Provides a conservative lower bound: some functions may be safe in practice but flagged due to library loading patterns

### 10.2 `sys.audit` hook in Worker

Python 3.8+ provides audit hooks via `sys.addaudithook()`. The hook is permanent (cannot be removed) and catches Python-level events.

```python
# Installed at Worker startup, before any bridge calls

import sys

_BLOCKED_EVENTS = {
    "open",          # file open
    "socket.connect", # outbound network
    "socket.bind",
    "subprocess.Popen",
    "os.system",
    "os.exec",
    "ctypes.dlopen",
    "compile",       # dynamic code compilation
    "exec",          # dynamic execution
    "import",        # import of new modules (beyond preloaded)
}

_AUDIT_LOG = []

def _audit_hook(event, args):
    if event in _BLOCKED_EVENTS:
        _AUDIT_LOG.append((event, args))
        raise RuntimeError(f"Blocked audit event: {event}")

sys.addaudithook(_audit_hook)
```

#### Advantages over strace

- Runs at Python level — catches intent, not just syscalls
- Works inside Lambda (no kernel support needed)
- Permanent — cannot be bypassed by library code
- Low overhead (~1us per event)

#### Limitations

- Only catches Python-level events. C extension code that directly calls libc functions bypasses audit hooks.
- For numpy/scipy, most dangerous operations (file I/O in `np.load`) do go through Python, so coverage is adequate.
- The hook is **defense in depth**, not the primary security boundary (WASM SFI is).

### 10.3 Parameter signature inspection

Some functions are safe in general but dangerous with specific arguments. For example, `np.loadtxt("data.csv")` requires file I/O, but `np.dot(a, b)` never does.

The Go dispatcher can inspect arguments before forwarding:

```go
// In shimmy_call, after filter check passes

func inspectArgs(funcName string, args []byte) error {
    // Heuristic: if any string argument looks like a file path, flag it
    var parsed []interface{}
    json.Unmarshal(args, &parsed)

    for _, arg := range parsed {
        if s, ok := arg.(string); ok {
            if looksLikeFilePath(s) {
                return fmt.Errorf("function %q received file-path-like argument %q", funcName, s)
            }
        }
    }
    return nil
}

func looksLikeFilePath(s string) bool {
    // Heuristic: starts with /, ~, or contains path separators with extensions
    if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") {
        return true
    }
    if strings.Contains(s, "/") && strings.Contains(s, ".") {
        return true
    }
    return false
}
```

This is a **heuristic defense** — it catches obvious cases but can be bypassed with creative encoding. It is intended as a warning system (log + flag for review) rather than a hard block.

### 10.4 Combined approach

The three approaches complement each other:

```
Layer 1: Offline strace profiling → generates base YAML whitelist
Layer 2: sys.audit hook → catches Python-level file/network/exec at runtime
Layer 3: Parameter inspection → flags suspicious arguments before execution

All layers are advisory. The security boundary remains WASM SFI.
```

---

## 11. Security Analysis

### 11.1 Threat model

Chrysalis assumes the following attacker model:

- **Attacker**: Untrusted user submitting arbitrary Python code for execution
- **Goal**: Execute arbitrary code on the host, read host filesystem, exfiltrate data, DoS the service, or escape the sandbox
- **Capabilities**: Can submit any syntactically valid Python code; can craft arbitrary byte sequences for array data; can attempt to exploit bugs in CPython, wazero, or the bridge

### 11.2 Threat analysis (WASM path)

| Threat | Mitigation | Residual risk |
|---|---|---|
| **Memory corruption** | WASM SFI: all memory accesses bounds-checked. Out-of-bounds traps. | Bug in wazero's bounds checking. wazero is well-tested and actively maintained. |
| **Arbitrary syscall execution** | WASM cannot execute `syscall` instructions. All host interaction via 4 registered host functions. | Bug in wazero's code generation that allows native instruction injection. Extremely unlikely for a pure-Go runtime. |
| **CPU exhaustion** | `context.Context` with deadline. wazero checks cancellation at loop back-edges and function calls. | Tight loop without back-edge checks. wazero inserts checks at all loop headers — verified by design. |
| **Memory exhaustion** | WASM linear memory capped (e.g., 256 MiB). `memory.grow` beyond limit traps. | WASM memory is allocated from Go heap. Very large limits could pressure the Go runtime. Use reasonable limits (256-512 MiB). |
| **Filesystem escape** | WASI pre-opened directories: only `/sandbox` (read-only) and `/tmp` (writable, size-limited). No access to host paths. | Symlink traversal within WASI. wazero's WASI implementation does not follow symlinks outside pre-opened dirs. |
| **Network access** | No network host functions registered. WASM cannot open sockets. | Exploitation of shm naming to communicate with other processes. Mitigated by random instance IDs in shm names. |
| **Bridge function abuse** | Go dispatcher filter (per-instance `map[string]bool`). Only whitelisted functions reach worker. | Filter misconfiguration (overly permissive YAML). Mitigated by deny-by-default and review process. |
| **Callback stack overflow** | Depth limit (MaxCallbackDepth = 8). Go dispatcher rejects deeper calls. | 8 levels of nested callbacks still consume ~192 KB of stack. Acceptable. |
| **Shared memory corruption** | shm blocks have typed headers. Worker validates magic, version, shape, dtype before use. | Malformed header could cause Worker crash. Worker validates all fields and rejects invalid headers with an error (no undefined behavior in Python). |
| **Time-of-check-time-of-use on shm** | WASM instance is single-threaded. Between `shimmy_shm_alloc` and the worker reading, no WASM code runs (the call is synchronous). | If the Go host were to allow concurrent bridge calls from one instance, TOCTOU would apply. Current design is sequential per instance. |

### 11.3 Threat analysis (fork path)

| Threat | Mitigation | Residual risk |
|---|---|---|
| **Arbitrary code execution** | None. Full native Python. | Full escape possible. Fork path is not a security boundary. |
| **CPU exhaustion** | `RLIMIT_CPU` = 30s. Kernel sends SIGKILL at hard limit. | Strong mitigation. |
| **Memory exhaustion** | `RLIMIT_AS` = 512 MiB. Kernel denies `mmap`/`brk` beyond limit. | Strong mitigation. |
| **Fork bomb** | `RLIMIT_NPROC` = 0. Cannot create new processes. | Note: limit applies to UID, not PID. Other processes under same UID are affected. Use a dedicated UID. |
| **Filesystem access** | No restriction. | Can read Lambda runtime files, `/proc`, `/etc`, function code. |
| **Network access** | No restriction. | Can exfiltrate data to external servers. |
| **Syscall-level attacks** | No seccomp. Full syscall access. | Can exploit kernel vulnerabilities (mitigated by Lambda's outer seccomp + Firecracker VMM). |

### 11.4 Trust boundaries

```
┌─────────────────────────────────────────────────────┐
│  Untrusted: User Python code                        │
│  ┌───────────────────────────────────────────────┐  │
│  │  WASM Sandbox (SFI boundary)                  │  │
│  │  CPython-WASI + user code                     │  │
│  └──────────────────┬────────────────────────────┘  │
│                     │ 4 host functions only          │
│  ┌──────────────────▼────────────────────────────┐  │
│  │  Trusted: Go Dispatcher                       │  │
│  │  Filter, shm management, callback routing     │  │
│  └──────────────────┬────────────────────────────┘  │
│                     │ Unix socket + shm              │
│  ┌──────────────────▼────────────────────────────┐  │
│  │  Semi-trusted: Python Worker                  │  │
│  │  Executes whitelisted functions only           │  │
│  │  (audit hook as defense-in-depth, future)     │  │
│  └───────────────────────────────────────────────┘  │
│                                                     │
│  Lambda Function Boundary                           │
└─────────────────────────────────────────────────────┘
│
│  Firecracker microVM boundary (not our concern)
│
```

### 11.5 Known gaps and mitigations

**Gap 1: wazero bugs**

wazero is the single point of trust for the WASM path. A bug in its SFI implementation would compromise isolation. Mitigation:
- wazero is a mature, well-tested project (1000+ GitHub stars, used in production by multiple companies)
- Fuzz testing is actively maintained
- The project has a security policy and responsive maintainers
- Consider periodic security audits of wazero's code generation

**Gap 2: Python worker escape**

If a whitelisted numpy/scipy function has an unexpected side effect (e.g., writes to a temp file, opens a network connection), the worker will execute it. Mitigation:
- Conservative whitelists (deny by default)
- Future: `sys.audit` hook catches Python-level file/network/exec
- Future: strace profiling validates whitelist entries
- The worker runs as the same user as the Lambda function — its privileges are already minimal

**Gap 3: Denial of service via bridge**

A malicious user could flood the bridge with calls, exhausting shm or socket buffers. Mitigation:
- Rate limit bridge calls per request (e.g., 10,000 calls max)
- Limit total shm allocation per request (e.g., 512 MiB)
- `context.Context` deadline applies to the entire request, including bridge time

**Gap 4: Information leakage via timing**

Bridge call timing may leak information about the worker's state (cache contents, BLAS implementation). This is not a significant concern for the target use case (autograding, data processing) but should be noted for future security-sensitive applications.

---

## 12. Performance Analysis

### 12.1 Latency budget

| Phase | Latency | Frequency | Notes |
|---|---|---|---|
| Lambda cold start | 500ms-2s | First invocation only | Dependent on deployment package size |
| WASM compilation (AOT) | 1-3s | Cold start only | CPython-WASI binary is ~10-15 MiB |
| Python worker startup | 1-2s | Cold start only | `import numpy` dominates |
| **Total cold start** | **3-7s** | **Once** | |
| Pool acquisition | <1ms | Every request | Channel receive |
| CPython-WASI init (pool miss) | 60-250ms | Pool empty only | Full interpreter bootstrap |
| Code safety pre-check | 1-5ms | Every request | AST parse + pattern match |
| User code execution | 10-500ms | Every request | Depends on code |
| Bridge call RTT | 0.5-2ms | Per bridge call | JSON encode/decode + socket |
| shm alloc + copy (1 MB array) | <0.1ms | Per array argument | memcpy speed |
| shm alloc + copy (100 MB array) | ~10ms | Rare | memcpy speed |
| Pool replenishment | 60-250ms | Async, after each request | Does not block response |
| **Total hot request (simple)** | **15-50ms** | | No bridge calls |
| **Total hot request (numpy)** | **20-100ms** | | 5-10 bridge calls typical |

### 12.2 Bottleneck analysis

**Bottleneck 1: CPython-WASI initialization (60-250ms)**

This is the dominant per-request cost when the pool is empty. The wide range depends on:
- Number of built-in modules loaded
- `site.py` processing
- shimmy bootstrap shim injection

Mitigations:
- Instance pool (amortizes init across requests)
- Memory snapshot restore (future: 10-20ms instead of 60-250ms)
- Minimize `site.py` processing (use `python3 -S` equivalent in WASI)

**Bottleneck 2: Bridge call serialization (0.3-1ms per call)**

JSON serialization of arguments and results adds latency to each bridge call. For code that makes many small calls (e.g., element-wise operations in a loop), this dominates.

```python
# Pathological case: 10,000 scalar operations via bridge
for i in range(10000):
    x = np.sin(i)  # Each call: ~1ms RTT → 10 seconds total
```

Mitigations:
- Batch operations when possible (the user should write `np.sin(np.arange(10000))`)
- Consider a fast binary serialization format (MessagePack, flatbuffers) if JSON proves too slow
- Probe caching: cache `__probe__` results so attribute access doesn't incur a bridge call

**Bottleneck 3: WASM overhead vs native Python**

CPython-WASI running in wazero is slower than native CPython. Expected overhead: 2-5x for pure Python code (compute-bound). This is acceptable because:
- Most compute-heavy work is offloaded to numpy/scipy via the bridge
- The bridge runs native code at full speed
- The WASM overhead applies only to Python control flow (loops, conditionals, object creation)

**Bottleneck 4: Cold start (3-7s)**

Unavoidable on first invocation. Mitigations:
- Provisioned concurrency (Lambda feature — keeps containers warm)
- Aggressive instance pooling on warm containers
- WASM compilation cache (wazero supports caching compiled modules to disk)

### 12.3 Throughput analysis

With a single Python worker (sequential requests):

```
Requests/sec = 1 / (pool_acquire + user_code + bridge_calls + pool_release)
             = 1 / (0.001 + 0.050 + 0.010 + 0.001)
             ≈ 16 requests/sec (per warm Lambda instance)
```

With pool replenishment pipelining (replenish starts before response):

```
Effective RPS ≈ 16-20 requests/sec (overlap hides replenishment latency)
```

This is adequate for the target use case. Lambda scales horizontally — throughput scales linearly with concurrency.

### 12.4 Memory budget

| Component | Memory | Notes |
|---|---|---|
| Go handler | 30-50 MiB | Runtime, wazero engine, compiled module |
| WASM instance (each) | 64-256 MiB | WASM linear memory (configurable) |
| Instance pool (4 instances) | 256-1024 MiB | Dominates memory usage |
| Python worker | 80-150 MiB | numpy/scipy/pandas preloaded |
| shm blocks | 0-512 MiB | Per-request, freed after use |
| **Total (steady state)** | **430-1788 MiB** | |

Lambda provides up to 10,240 MiB of memory. A pool of 4 instances with 256 MiB each fits comfortably in a 2048 MiB Lambda function. Adjust pool size and memory limits based on the Lambda memory configuration.

### 12.5 Optimization roadmap

| Optimization | Expected improvement | Complexity | Phase |
|---|---|---|---|
| Instance pool | 60-250ms → <1ms per request | Low | Phase 1 |
| Probe caching | Eliminate repeated __probe__ calls | Low | Phase 1 |
| Binary serialization (MessagePack) | 30-50% faster than JSON | Medium | Phase 2 |
| Memory snapshot/restore | 60-250ms → 10-20ms init | High | Phase 3 |
| Batch bridge calls | Amortize RTT across multiple ops | Medium | Phase 2 |
| Compiled module cache (disk) | 1-3s → <100ms WASM compile | Low | Phase 1 |
| Worker pool (2-4 workers) | 2-4x throughput | Low | Phase 2 |

---

## 13. Implementation Roadmap

### Phase 1: MVP (Minimum Viable Product)

**Goal**: End-to-end execution of pure Python code in the WASM sandbox, with basic numpy bridge support.

**Duration**: 6-8 weeks

#### Milestones

1. **CPython-WASI build** (Week 1-2)
   - Compile CPython 3.11+ to WASI using wasi-sdk
   - Verify basic Python execution (print, math, string ops)
   - Minimal `site.py` for fast startup
   - Export `__shimmy_alloc` and `__shimmy_invoke_cb` functions

2. **wazero host integration** (Week 2-3)
   - Go handler with wazero runtime
   - Register 4 host functions (`shimmy_call`, `shimmy_shm_alloc`, `shimmy_shm_free`, `shimmy_invoke_callback`)
   - WASI filesystem configuration (pre-opened `/sandbox`, `/tmp`)
   - Context deadline for CPU limits
   - CompiledModule caching

3. **Python worker** (Week 3-4)
   - Worker subprocess with Unix domain socket
   - Length-prefixed JSON protocol
   - numpy/scipy preloading
   - Function dispatch (`call`, `probe`, `ping`, `shutdown`)
   - shm read/write with numpy integration

4. **Import bridge shim** (Week 4-5)
   - `_ShimmyFinder`, `_ShimmyLoader`, `_ProxyModule`, `_BridgedFunction`
   - Array serialization via shm
   - Scalar/list/dict serialization via JSON
   - Basic error propagation

5. **Filter system** (Week 5-6)
   - YAML profile loading
   - `map[string]bool` filter with wildcard support
   - Default profiles: `autograder.yaml`, `data-processing.yaml`, `math-only.yaml`
   - Per-instance filter binding

6. **Instance pool** (Week 6-7)
   - Buffered channel pool
   - Pre-fill at cold start
   - Async replenishment
   - Pool size configuration

7. **Code safety pre-check** (Week 7)
   - AST parser for Python code (Go implementation or call Python AST)
   - Block list: `os`, `sys`, `socket`, `subprocess`, `ctypes`, `importlib`, `pickle`, `marshal`
   - Block builtins: `eval`, `exec`, `compile`, `__import__`, `open`

8. **Lambda integration** (Week 7-8)
   - Lambda handler function
   - API Gateway integration
   - Error handling and response formatting
   - CloudWatch logging
   - Basic load testing

#### Phase 1 deliverables

- Lambda function deployable via SAM/CDK
- Supports pure Python + numpy core operations (array creation, math, linalg)
- 3 filter profiles
- <100ms hot latency for simple numpy operations
- <7s cold start

### Phase 2: Production hardening

**Goal**: Bidirectional callbacks, fork path, reliability, observability.

**Duration**: 4-6 weeks

#### Milestones

1. **Bidirectional callbacks** (Week 1-2)
   - Callback registration in WASM shim
   - `CallbackProxy` in Worker
   - `shimmy_invoke_callback` host function
   - Depth limit enforcement
   - Callback cleanup on call completion
   - Test with `scipy.optimize.minimize`

2. **Fork path** (Week 2-3)
   - `fork+exec` with `setrlimit`
   - Process management (waitpid, timeout, signal handling)
   - Stdout/stderr capture
   - Execution path routing based on task config

3. **Reliability** (Week 3-4)
   - Worker health checking (periodic pings)
   - Worker restart on crash
   - Instance pool drain and refill on errors
   - Graceful shutdown
   - Request timeout handling (context cancellation propagation)

4. **Observability** (Week 4-5)
   - Structured logging (JSON logs to CloudWatch)
   - Metrics: request latency, bridge call count, pool utilization, shm usage
   - X-Ray tracing integration
   - Error classification and alerting

5. **Performance optimization** (Week 5-6)
   - Probe caching (eliminate repeated `__probe__` calls)
   - Compiled module disk cache
   - Batch bridge call support
   - MessagePack serialization (evaluate vs JSON)
   - Worker pool (2-4 workers)

#### Phase 2 deliverables

- Full scipy.optimize support (via callbacks)
- Fork path as fallback
- Production-grade error handling and observability
- <50ms hot latency for typical numpy workloads

### Phase 3: Advanced features

**Goal**: Smart filtering, memory snapshots, pandas support, multi-tenant.

**Duration**: 6-8 weeks

#### Milestones

1. **Smart filter: strace profiling** (Week 1-2)
   - Profiling harness for numpy/scipy API surface
   - Syscall classification pipeline
   - Auto-generated YAML whitelist
   - Human review workflow

2. **Smart filter: sys.audit hook** (Week 2-3)
   - Audit hook installation in Worker
   - Event classification and blocking
   - Audit log aggregation

3. **Smart filter: parameter inspection** (Week 3-4)
   - File path heuristic in Go dispatcher
   - Configurable inspection rules
   - Logging and alerting for flagged calls

4. **Memory snapshot/restore** (Week 4-6)
   - WASM linear memory snapshot after CPython init
   - Snapshot storage (in-memory or memory-mapped file)
   - Restore path (memcpy or userfaultfd)
   - Benchmark: verify 10-20ms restore time
   - CPython state reinitialize after restore

5. **Pandas bridge** (Week 6-7)
   - DataFrame proxy object
   - Column access, filtering, aggregation via bridge
   - shm transfer for DataFrame backing arrays
   - Common pandas operations whitelisted

6. **Multi-tenant hardening** (Week 7-8)
   - Per-tenant resource quotas
   - Tenant isolation in shm naming
   - Rate limiting per tenant
   - Audit logging per tenant

#### Phase 3 deliverables

- Auto-generated filter profiles
- <20ms instance init via memory snapshots
- Full pandas support
- Multi-tenant deployment model

---

## Appendix A: Code Safety Pre-check Implementation

The static analysis pre-check is implemented in Go for speed. It parses a Python AST (using a minimal Python parser or by invoking `python3 -c "import ast; ..."`) and checks for blocked patterns.

```go
type SafetyCheckResult struct {
    Safe     bool     `json:"safe"`
    Warnings []string `json:"warnings"`
    Blocked  []string `json:"blocked"`
}

// Blocked module imports
var blockedImports = map[string]bool{
    "os":         true,
    "sys":        true,
    "socket":     true,
    "subprocess": true,
    "ctypes":     true,
    "importlib":  true,
    "pickle":     true,
    "marshal":    true,
    "shutil":     true,
    "pathlib":    true,
    "io":         true,
    "tempfile":   true,
    "signal":     true,
    "multiprocessing": true,
    "threading":  true,
}

// Blocked builtin calls
var blockedBuiltins = map[string]bool{
    "eval":       true,
    "exec":       true,
    "compile":    true,
    "__import__": true,
    "open":       true,
    "breakpoint": true,
    "globals":    true,
    "locals":     true,
    "vars":       true,
    "dir":        true,
    "getattr":    true,
    "setattr":    true,
    "delattr":    true,
}

func CheckCodeSafety(code string) SafetyCheckResult {
    result := SafetyCheckResult{Safe: true}

    // Parse AST (invoke Python for accuracy)
    ast, err := parseAST(code)
    if err != nil {
        result.Safe = false
        result.Blocked = append(result.Blocked, "Failed to parse: "+err.Error())
        return result
    }

    // Walk AST looking for violations
    walkAST(ast, func(node ASTNode) {
        switch n := node.(type) {
        case *ImportNode:
            if blockedImports[n.Module] {
                result.Safe = false
                result.Blocked = append(result.Blocked,
                    fmt.Sprintf("line %d: import of blocked module %q", n.Line, n.Module))
            }
        case *CallNode:
            if blockedBuiltins[n.FuncName] {
                result.Safe = false
                result.Blocked = append(result.Blocked,
                    fmt.Sprintf("line %d: call to blocked builtin %q", n.Line, n.FuncName))
            }
        case *AttributeNode:
            // Check for __dunder__ access patterns used to escape sandboxes
            if strings.HasPrefix(n.Attr, "__") && strings.HasSuffix(n.Attr, "__") {
                if n.Attr != "__init__" && n.Attr != "__str__" && n.Attr != "__repr__" {
                    result.Warnings = append(result.Warnings,
                        fmt.Sprintf("line %d: dunder access %q", n.Line, n.Attr))
                }
            }
        }
    })

    return result
}
```

This pre-check is a **fast filter**, not a security boundary. Obfuscation techniques (string concatenation to build module names, `getattr` chains, encoding tricks) can bypass it. The WASM SFI boundary is the actual security mechanism.

---

## Appendix B: shm dtype enumeration

```go
const (
    DTypeFloat32  uint32 = 0
    DTypeFloat64  uint32 = 1
    DTypeInt32    uint32 = 2
    DTypeInt64    uint32 = 3
    DTypeUint8    uint32 = 4
    DTypeUint16   uint32 = 5
    DTypeUint32   uint32 = 6
    DTypeUint64   uint32 = 7
    DTypeInt8     uint32 = 8
    DTypeInt16    uint32 = 9
    DTypeFloat16  uint32 = 10
    DTypeBool     uint32 = 11
    DTypeComplex64  uint32 = 12
    DTypeComplex128 uint32 = 13
)

var dtypeItemSize = map[uint32]uint32{
    DTypeFloat32:    4,
    DTypeFloat64:    8,
    DTypeInt32:      4,
    DTypeInt64:      8,
    DTypeUint8:      1,
    DTypeUint16:     2,
    DTypeUint32:     4,
    DTypeUint64:     8,
    DTypeInt8:       1,
    DTypeInt16:      2,
    DTypeFloat16:    2,
    DTypeBool:       1,
    DTypeComplex64:  8,
    DTypeComplex128: 16,
}

var dtypeToNumpy = map[uint32]string{
    DTypeFloat32:    "float32",
    DTypeFloat64:    "float64",
    DTypeInt32:      "int32",
    DTypeInt64:      "int64",
    DTypeUint8:      "uint8",
    DTypeUint16:     "uint16",
    DTypeUint32:     "uint32",
    DTypeUint64:     "uint64",
    DTypeInt8:       "int8",
    DTypeInt16:      "int16",
    DTypeFloat16:    "float16",
    DTypeBool:       "bool",
    DTypeComplex64:  "complex64",
    DTypeComplex128: "complex128",
}
```

---

## Appendix C: Error handling strategy

### Error categories

| Category | Example | Handling |
|---|---|---|
| **User code error** | `NameError: name 'x' is not defined` | Return to user as execution result |
| **Bridge error** | Function not in filter whitelist | Return to user as execution error |
| **Worker error** | numpy raises `LinAlgError` | Return to user as library error |
| **System error** | Worker crashed, shm allocation failed | Log, restart worker, retry or return 500 |
| **Timeout** | Context deadline exceeded | Kill execution, return timeout error |
| **Safety check failure** | Blocked import detected | Return to user before execution |

### Error propagation

```
User code error → CPython exception → WASM shim catches → shimmy_call returns error JSON
                                                          → Go reads error → returns to caller

Bridge filter deny → Go dispatcher → returns error JSON to WASM → shim raises RuntimeError

Worker exception → Worker sends error response on socket → Go reads → writes error to WASM
                                                                      → shim raises RuntimeError

Worker crash → Go detects broken socket → restarts worker → returns system error to caller

Context timeout → Go cancels context → wazero traps WASM execution → Go returns timeout error
```

### Structured error format (API response)

```json
{
    "request_id": "abc-123",
    "status": "error",
    "error": {
        "type": "execution_error",
        "category": "user_code",
        "message": "NameError: name 'x' is not defined",
        "traceback": "  File \"<user>\", line 3, in <module>\n    print(x)\nNameError: ...",
        "line": 3
    },
    "timing": {
        "total_ms": 45,
        "init_ms": 0,
        "exec_ms": 42,
        "bridge_calls": 0,
        "bridge_ms": 0
    }
}
```

---

## Appendix D: Configuration reference

### Lambda environment variables

| Variable | Default | Description |
|---|---|---|
| `CHRYSALIS_POOL_SIZE` | `4` | Number of pre-initialized WASM instances |
| `CHRYSALIS_MAX_MEMORY_MB` | `256` | WASM linear memory limit per instance |
| `CHRYSALIS_TIMEOUT_SEC` | `30` | Default execution timeout |
| `CHRYSALIS_WORKER_SOCKET` | `/tmp/chrysalis_worker.sock` | Worker Unix socket path |
| `CHRYSALIS_FILTER_DIR` | `./filters` | Directory containing YAML filter profiles |
| `CHRYSALIS_DEFAULT_FILTER` | `autograder` | Default filter profile |
| `CHRYSALIS_LOG_LEVEL` | `info` | Log level (debug, info, warn, error) |
| `CHRYSALIS_MAX_BRIDGE_CALLS` | `10000` | Maximum bridge calls per request |
| `CHRYSALIS_MAX_SHM_MB` | `512` | Maximum shm allocation per request |
| `CHRYSALIS_MAX_CALLBACK_DEPTH` | `8` | Maximum callback nesting depth |
| `CHRYSALIS_WASM_CACHE_DIR` | `/tmp/chrysalis_cache` | Compiled module cache directory |

### Task submission format

```json
{
    "code": "import numpy as np\nresult = np.dot(np.array([1,2,3]), np.array([4,5,6]))\nprint(result)",
    "path": "wasm",
    "filter_profile": "autograder",
    "timeout_sec": 30,
    "max_memory_mb": 256,
    "stdin": "",
    "env": {},
    "metadata": {
        "user_id": "student-42",
        "assignment_id": "hw3-q2"
    }
}
```

### Task result format

```json
{
    "request_id": "abc-123",
    "status": "success",
    "stdout": "32\n",
    "stderr": "",
    "return_value": null,
    "timing": {
        "total_ms": 23,
        "init_ms": 0,
        "exec_ms": 18,
        "bridge_calls": 3,
        "bridge_ms": 5
    },
    "resource_usage": {
        "peak_memory_mb": 12,
        "shm_allocated_mb": 0.1,
        "instances_available": 3
    }
}
```
