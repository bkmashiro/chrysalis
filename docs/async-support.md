# Async Support in Chrysalis

> **Status**: Design draft v1 (2026-05-07)  
> **Context**: The core bridge protocol is synchronous (blocking bridge calls via stdin/stdout pipes). This document covers how async Python code interacts with the bridge and what Chrysalis needs to handle correctly.

---

## The Core Tension

The bridge call is **synchronous by nature**:

```
Python (in WASM) calls __shimmy_call__(req)
  → blocks waiting for Go bridge goroutine
  → Go forwards to Worker, waits for result
  → writes response back
  → Python unblocks
```

Async Python code runs on a **single-threaded event loop** (asyncio). When the event loop hits an `await`, it suspends the current coroutine and runs other ready coroutines. A *blocking* call (like the bridge) stalls the entire event loop — but in the WASM sandbox this is fine because:

1. There are no other coroutines waiting on I/O (no real network, no real filesystem)
2. User code is the only thing running in the WASM instance
3. "Stalling the event loop" just means waiting for the numpy result, which is expected

In practice, this means **asyncio + numpy bridge works out of the box** for the common case.

---

## Case Analysis

### Case 1: async function with numpy calls ✅ Works as-is

```python
import asyncio
import numpy as np

async def compute(matrix):
    # np.linalg.eig is a synchronous bridge call — blocks briefly, fine
    eigenvalues, _ = np.linalg.eig(matrix)
    return eigenvalues

result = asyncio.run(compute(np.eye(3)))
```

The `np.linalg.eig` call blocks the event loop for the duration of the bridge RTT (~1-2ms). Since no other coroutines are waiting, this is identical to the sync case.

**Action needed**: Remove `asyncio` from the safety checker deny list. It is not inherently dangerous. The sandbox boundary is WASM SFI, not import blocking.

### Case 2: async functions as callbacks ⚠️ Needs wrapping

```python
import asyncio
import numpy as np
from scipy.optimize import minimize  # via bridge

async def objective(x):
    # async because user may have await inside
    return float(np.dot(x, x))

# Problem: scipy will call objective(x_i), get a coroutine object, not a float
result = minimize(objective, [1.0, 2.0])
```

The bridge receives a `callback` argument. When it serializes the callable, it must detect that it's a coroutine function and wrap it into a synchronous function that drives the event loop internally.

**Fix in bootstrap.py**:

```python
import inspect

def _maybe_sync(fn):
    """
    If fn is an async function, wrap it so callers get the result directly.
    Uses a private event loop to avoid nesting issues with asyncio.run().
    """
    if not inspect.iscoroutinefunction(fn):
        return fn

    import asyncio

    def _sync_wrapper(*args, **kwargs):
        try:
            # If there's already a running loop (we're inside asyncio.run()),
            # we can't call asyncio.run() again — use a new thread's loop instead
            loop = asyncio.get_running_loop()
        except RuntimeError:
            loop = None

        if loop is not None and loop.is_running():
            # Running inside an event loop: use run_until_complete on a new loop
            # in a separate thread to avoid "cannot be called from a running loop"
            import concurrent.futures
            with concurrent.futures.ThreadPoolExecutor(max_workers=1) as ex:
                future = ex.submit(asyncio.run, fn(*args, **kwargs))
                return future.result()
        else:
            return asyncio.run(fn(*args, **kwargs))

    _sync_wrapper.__name__ = getattr(fn, '__name__', 'async_callback')
    return _sync_wrapper
```

Applied when registering callbacks:

```python
def _serialize_one(arg):
    ...
    elif callable(arg):
        cb_id = _register_callback(_maybe_sync(arg))
        return {'kind': 'callback', 'cb_id': cb_id}
```

### Case 3: Awaiting bridge results (async numpy API) 🔮 Future / opt-in

Some users may want to write:

```python
result = await np.dot(a, b)  # hypothetical async numpy
```

This is not standard numpy API and not something users expect. Don't implement this — users write `np.dot(a, b)` (sync), which works inside any async function. This case is a non-issue.

### Case 4: async generators ⚠️ Proxy needs `__aiter__`/`__anext__`

```python
async def process_stream():
    async for row in dataset.iterrows():  # pandas async gen via bridge
        result = np.dot(row, weights)
        yield result
```

The proxy module returned for `pandas` doesn't implement `__aiter__`/`__anext__`, so `async for x in proxy_obj` will raise `TypeError`.

**Fix**: Add async iteration support to the proxy:

```python
class _AsyncIterProxy:
    """Wraps a sync bridge call result that should be async-iterable."""
    def __init__(self, items):
        self._items = iter(items)

    def __aiter__(self):
        return self

    async def __anext__(self):
        try:
            return next(self._items)
        except StopIteration:
            raise StopAsyncIteration


class _Proxy:
    ...
    def __aiter__(self):
        # Materialize the iterable synchronously, wrap in async iterator
        items = _bridge_call(self.__ns + '.__iter__', (), {})
        return _AsyncIterProxy(items)
```

This is a **Phase 2** item — needed for pandas iteration patterns, not for numpy.

### Case 5: asyncio.gather / concurrent numpy calls 🔴 Won't work

```python
async def main():
    # Run two numpy operations concurrently?
    a, b = await asyncio.gather(
        asyncio.to_thread(np.linalg.eig, matrix1),
        asyncio.to_thread(np.linalg.svd, matrix2),
    )
```

`asyncio.to_thread` spins up a real thread. Threads don't exist in CPython-WASI (WASI has no `pthread`). This will fail at the WASM level.

Even if it didn't: the bridge socket is not thread-safe — concurrent bridge calls would interleave on the wire.

**Behaviour**: The safety checker blocks `threading`. `asyncio.to_thread` internally uses `concurrent.futures.ThreadPoolExecutor` which uses `threading` — also blocked. This fails safely.

**User-visible error**: `RuntimeError: threading not available in Chrysalis sandbox` (raised by the import block, before any damage).

---

## Changes Required

### 1. Safety checker: allow asyncio

Remove from deny list:
- `asyncio`
- `concurrent.futures` (it's needed by the async callback wrapper)

Keep denied:
- `threading` (raw threads, not safe)
- `multiprocessing` (fork, not safe in WASM)

Updated deny list rationale:

| Module | Status | Reason |
|---|---|---|
| `asyncio` | ✅ Allow | Single-threaded event loop, no syscall risk |
| `concurrent.futures` | ✅ Allow | Used by async callback wrapper; ThreadPoolExecutor will fail at WASM level if misused |
| `threading` | ❌ Deny | Raw threads, WASM has no pthreads |
| `multiprocessing` | ❌ Deny | Fork, not available in WASM |

### 2. bootstrap.py: add `_maybe_sync` wrapper

Applied to all callable arguments before registering as callbacks (see Case 2 above).

### 3. bootstrap.py: add `__aiter__`/`__anext__` to `_Proxy`

Phase 2, needed for pandas iteration. Basic version:

```python
class _Proxy:
    ...
    def __aiter__(self):
        raise TypeError(
            f"'{self.__ns}' async iteration not yet supported in Chrysalis. "
            f"Use a regular for loop or list() instead."
        )
```

Give a clear error now, implement properly in Phase 2.

---

## Event Loop Lifecycle

One subtle issue: `asyncio.run()` creates a **new event loop**, runs until complete, then **closes and discards** the loop. If user code calls `asyncio.run(main())` and `main()` internally triggers async callbacks that call `asyncio.run()` again (via `_maybe_sync`), we get a `RuntimeError: asyncio.run() cannot be called from a running event loop`.

The `_maybe_sync` wrapper handles this with the thread-based fallback (see Case 2 code). For maximum safety, add a depth counter:

```python
_callback_depth = 0
_MAX_CALLBACK_DEPTH = 8  # matches CHRYSALIS_MAX_CALLBACK_DEPTH config

def _register_callback(fn):
    global _next_cb_id
    cb_id = _next_cb_id
    _next_cb_id += 1

    def _guarded(*args, **kwargs):
        global _callback_depth
        if _callback_depth >= _MAX_CALLBACK_DEPTH:
            raise RecursionError("Chrysalis: maximum callback depth exceeded")
        _callback_depth += 1
        try:
            return fn(*args, **kwargs)
        finally:
            _callback_depth -= 1

    _callbacks[cb_id] = _guarded
    return cb_id
```

---

## Summary Table

| Scenario | Works? | What to do |
|---|---|---|
| `async def f(): np.dot(...)` | ✅ Already works | Allow `asyncio` in safety checker |
| `asyncio.run(main())` | ✅ Already works | Allow `asyncio` |
| `scipy.minimize(async_fn, x0)` | ⚠️ Needs fix | `_maybe_sync` wrapper on callbacks |
| `async for x in proxy:` | ⚠️ Phase 2 | Return clear error for now |
| `asyncio.gather(to_thread(...))` | 🔴 Won't work | Fails safely (threading denied) |
| `asyncio.gather(coro1, coro2)` | ✅ Works | Both coroutines share the event loop, bridge calls block briefly |

---

## Implementation Checklist

- [ ] Remove `asyncio` and `concurrent.futures` from safety checker deny list
- [ ] Add `_maybe_sync` to `bootstrap.py` callback registration
- [ ] Add `__aiter__` stub with clear error message to `_Proxy`
- [ ] Add callback depth counter to `_register_callback`
- [ ] Test: `asyncio.run()` with numpy calls
- [ ] Test: `scipy.optimize.minimize` with `async def` objective
- [ ] Test: nested `asyncio.run()` (should use thread fallback, not crash)
- [ ] Phase 2: proper `__aiter__`/`__anext__` for pandas iteration
