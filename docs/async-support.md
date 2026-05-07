# Async Support in Chrysalis

> **Status**: Design draft v2 (2026-05-07)  
> **Context**: The core bridge protocol is synchronous (blocking bridge calls via stdin/stdout pipes). This document covers how async Python code interacts with the bridge and what Chrysalis needs to handle correctly.
>
> **Design goal**: Full transparent async support — user code runs unmodified whether it uses sync or async patterns. No user-facing API changes required.

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

Async Python code runs on a **single-threaded event loop** (asyncio). When the event loop hits an `await`, it suspends the current coroutine and runs other ready coroutines. A blocking call stalls the event loop — but in the WASM sandbox this is fine because:

1. There are no other coroutines waiting on real I/O (no real network/filesystem)
2. User code is the only tenant of the WASM instance
3. "Stalling the event loop" = waiting for the numpy result, which is expected

This means **asyncio + numpy bridge works out of the box for the common case**. The transparency mechanism described below handles the remaining edge cases.

---

## Transparent Async: Design

### 1. `_BridgeResult` — a value that is also awaitable

Every bridge call returns a `_BridgeResult` instead of a raw value. It behaves like the actual value in sync context, and resolves immediately in async context:

```python
class _BridgeResult:
    """
    Transparent wrapper around a bridge call result.
    - Used directly: acts as the underlying value (delegates all operators)
    - Awaited: resolves immediately with the value (no actual suspension)

    This makes the following both valid and equivalent:
        result = np.dot(a, b)           # sync
        result = await np.dot(a, b)     # async — same bridge call, same result
    """
    __slots__ = ('_v',)

    def __init__(self, value):
        self._v = value

    # ── Async protocol ────────────────────────────────────────────────────────
    def __await__(self):
        # No actual suspension — the value is already here.
        # yield makes this a valid generator-based coroutine.
        return self._v
        yield  # unreachable, but makes __await__ a generator function

    # ── Transparent numeric operators ─────────────────────────────────────────
    def __repr__(self):   return repr(self._v)
    def __str__(self):    return str(self._v)
    def __int__(self):    return int(self._v)
    def __float__(self):  return float(self._v)
    def __bool__(self):   return bool(self._v)
    def __len__(self):    return len(self._v)
    def __iter__(self):   return iter(self._v)
    def __getitem__(self, k): return self._v[k]
    def __setitem__(self, k, v): self._v[k] = v

    def __add__(self, o):  return self._v + o
    def __radd__(self, o): return o + self._v
    def __sub__(self, o):  return self._v - o
    def __rsub__(self, o): return o - self._v
    def __mul__(self, o):  return self._v * o
    def __rmul__(self, o): return o * self._v
    def __truediv__(self, o):  return self._v / o
    def __rtruediv__(self, o): return o / self._v
    def __matmul__(self, o):   return self._v @ o
    def __rmatmul__(self, o):  return o @ self._v
    def __neg__(self):  return -self._v
    def __pos__(self):  return +self._v
    def __abs__(self):  return abs(self._v)

    def __eq__(self, o):  return self._v == o
    def __lt__(self, o):  return self._v < o
    def __le__(self, o):  return self._v <= o
    def __gt__(self, o):  return self._v > o
    def __ge__(self, o):  return self._v >= o

    # Attribute access: forward to underlying value (e.g. array.shape, array.T)
    def __getattr__(self, name):
        return getattr(self._v, name)
```

### 2. `_drive_coroutine` — run async callbacks without asyncio

When a user passes an `async def` function as a callback (e.g. to `scipy.optimize.minimize`), the library will call it synchronously. We need to transparently resolve it to its return value without creating a nested event loop.

`asyncio.run()` cannot be called from a running event loop. Instead, drive the coroutine manually — this works for coroutines that don't actually suspend on external I/O (which is all coroutines in the WASM sandbox, since there is no real I/O):

```python
def _drive_coroutine(coro):
    """
    Drive a coroutine to completion without an event loop.

    Works because all coroutines in the WASM sandbox either:
    - Return immediately (pure computation)
    - Await other coroutines (which also return immediately)
    - Await bridge results (which are already resolved _BridgeResult objects)

    There is no real async I/O in the sandbox, so no coroutine
    ever truly suspends waiting for an external event.
    """
    try:
        # Send None to start the coroutine. If it completes immediately,
        # StopIteration carries the return value.
        coro.send(None)
        # If we get here, the coroutine yielded (suspended).
        # Keep driving until it finishes.
        while True:
            coro.send(None)
    except StopIteration as e:
        return e.value
    except Exception:
        coro.close()
        raise
```

### 3. `_maybe_sync` — transparent callable wrapping

Applied to all callable arguments at serialization time. Callers (scipy, etc.) always receive a sync function, regardless of whether the user wrote `def f` or `async def f`:

```python
import inspect

def _maybe_sync(fn):
    """
    If fn is an async function, return a sync wrapper that drives
    the coroutine to completion. Otherwise return fn unchanged.
    The wrapping is transparent — the wrapper has the same __name__.
    """
    if not inspect.iscoroutinefunction(fn):
        return fn

    def _sync_wrapper(*args, **kwargs):
        return _drive_coroutine(fn(*args, **kwargs))

    _sync_wrapper.__name__ = getattr(fn, '__name__', 'async_fn')
    _sync_wrapper.__wrapped__ = fn
    return _sync_wrapper
```

Applied in callback registration:

```python
def _serialize_one(arg):
    ...
    elif callable(arg):
        cb_id = _register_callback(_maybe_sync(arg))
        return {'kind': 'callback', 'cb_id': cb_id}
```

---

## Case Analysis

### Case 1: async function with numpy calls ✅ Fully transparent

```python
import asyncio
import numpy as np

async def compute(matrix):
    eigenvalues, _ = np.linalg.eig(matrix)   # sync bridge call, fine
    return eigenvalues

async def pipeline(data):
    results = []
    for batch in data:
        r = await np.dot(batch, batch.T)      # await _BridgeResult → immediate
        results.append(r)
    return results

asyncio.run(compute(np.eye(3)))
```

No changes needed in user code. Both `np.linalg.eig(matrix)` (sync use) and `await np.dot(...)` (async use) work identically.

### Case 2: async callbacks ✅ Fully transparent

```python
import numpy as np
from scipy.optimize import minimize  # via bridge

async def objective(x):
    # User wrote async def — maybe they have other awaits inside
    loss = float(np.dot(x, x))
    return loss

# scipy calls objective(x_i) synchronously — _maybe_sync makes this work
result = minimize(objective, [1.0, 2.0])
print(result.x)   # [0., 0.]
```

`_maybe_sync` detects `iscoroutinefunction(objective) == True` and wraps it. scipy sees a normal sync function. The coroutine is driven by `_drive_coroutine` which works correctly because the coroutine has no real async suspension points.

### Case 3: `await np.dot(...)` ✅ Fully transparent

```python
async def main():
    a = np.array([1.0, 2.0, 3.0])
    result = await np.dot(a, a)    # _BridgeResult.__await__ → immediate resolve
    print(result)                  # 14.0
```

`_BridgeResult.__await__` is a generator that immediately returns the value with no suspension. Equivalent to `result = np.dot(a, a)`.

### Case 4: `asyncio.gather` over coroutines ✅ Works

```python
async def step1(x): return np.sum(x)
async def step2(x): return np.mean(x)

async def main():
    a = np.arange(100.0)
    s, m = await asyncio.gather(step1(a), step2(a))
```

Both coroutines share the single-threaded event loop. Each bridge call blocks briefly (~1-2ms) while it runs. No parallelism occurs, but the code runs correctly. The event loop schedules them sequentially.

### Case 5: async generators ⚠️ Phase 2

```python
async for row in dataset.iterrows():   # pandas async gen via bridge
    result = np.dot(row, weights)
```

The proxy `_Proxy` doesn't implement `__aiter__`/`__anext__`. For now, raise a clear error:

```python
class _Proxy:
    def __aiter__(self):
        raise TypeError(
            f"Chrysalis: async iteration over '{self.__ns}' is not yet supported. "
            f"Use: for row in list({self.__ns}.iterrows()): ..."
        )
```

Full implementation is Phase 2 (needed for pandas patterns):

```python
class _AsyncIterProxy:
    def __init__(self, items):
        self._it = iter(items)

    def __aiter__(self):
        return self

    async def __anext__(self):
        try:
            return next(self._it)
        except StopIteration:
            raise StopAsyncIteration

class _Proxy:
    def __aiter__(self):
        # Materialize the full iterable via bridge, wrap in async iterator
        items = _bridge_call(self.__ns + '.__iter_all__', (), {})
        return _AsyncIterProxy(items)
```

### Case 6: True parallelism ❌ WASM limitation

```python
await asyncio.gather(
    asyncio.to_thread(np.linalg.eig, A),
    asyncio.to_thread(np.linalg.svd, B),
)
```

`asyncio.to_thread` requires real OS threads (`threading` module internally). WASM/WASI does not have `pthread`. This fails at the WASM runtime level, not at the Chrysalis level.

This is a **fundamental WebAssembly constraint**, not specific to Chrysalis. The WASI threads proposal (wasi-threads) exists but is not yet stable and CPython-WASI does not implement it.

**Behaviour**: `threading` remains in the deny list. `asyncio.to_thread` will raise `ImportError` (it depends on `threading`) before any damage is done. Error message should be clear:

```
ImportError: [Chrysalis] 'threading' is not available in the WASM sandbox.
True parallelism is not supported. Use sequential async patterns instead.
```

For CPU-bound parallelism needs, the recommended pattern is sequential batching or using the fork path (which runs native Python outside the sandbox).

---

## Safety Checker Changes

### Allow list updates

| Module | Before | After | Reason |
|---|---|---|---|
| `asyncio` | ❌ Deny | ✅ Allow | Single-threaded, no syscall risk |
| `concurrent.futures` | ❌ Deny | ✅ Allow | Needed by async patterns; ThreadPoolExecutor fails safely at WASM level |
| `threading` | ❌ Deny | ❌ Deny | No pthreads in WASM — WASM limitation |
| `multiprocessing` | ❌ Deny | ❌ Deny | No fork in WASM — WASM limitation |

The deny rationale for `threading` and `multiprocessing` changes from "security risk" to "WASM limitation" — they simply don't work, and would fail at the WASM runtime level even without the safety check. The safety check provides an earlier, cleaner error message.

---

## Callback Depth Protection

Async callbacks can be nested (e.g. scipy calls objective, which calls another scipy function, which calls another objective...). The existing callback depth counter applies:

```python
_callback_depth = 0
_MAX_CALLBACK_DEPTH = 8  # matches CHRYSALIS_MAX_CALLBACK_DEPTH env var

def _guarded_callback(fn):
    def _wrapper(*args, **kwargs):
        global _callback_depth
        if _callback_depth >= _MAX_CALLBACK_DEPTH:
            raise RecursionError(
                f"[Chrysalis] Maximum callback depth ({_MAX_CALLBACK_DEPTH}) exceeded. "
                f"Possible infinite recursion in user-provided callback."
            )
        _callback_depth += 1
        try:
            return fn(*args, **kwargs)
        finally:
            _callback_depth -= 1
    return _wrapper

def _register_callback(fn):
    cb_id = _next_cb_id
    ...
    _callbacks[cb_id] = _guarded_callback(_maybe_sync(fn))
    return cb_id
```

---

## Summary

| Scenario | Status | Mechanism |
|---|---|---|
| `async def f(): np.dot(...)` | ✅ Transparent | Bridge blocks briefly, event loop continues |
| `result = await np.dot(a, b)` | ✅ Transparent | `_BridgeResult.__await__` resolves immediately |
| `scipy.minimize(async_fn, x0)` | ✅ Transparent | `_maybe_sync` + `_drive_coroutine` |
| `asyncio.gather(coro1, coro2)` | ✅ Works | Sequential on single event loop |
| `async for x in proxy:` | ⚠️ Phase 2 | Clear error for now, full impl in Phase 2 |
| `asyncio.to_thread(...)` | ❌ WASM limit | No pthreads in WASM/WASI — not a Chrysalis choice |
| Raw `threading.Thread` | ❌ WASM limit | Same — denied for clean error message |

The key insight: **there is no real async I/O in the WASM sandbox**. Every `await` either resolves immediately (`_BridgeResult`) or awaits another coroutine that also resolves immediately. `_drive_coroutine` works correctly under this constraint. True parallelism requires real threads, which WASM does not provide — this is a platform constraint, not a design tradeoff.

---

## Implementation Checklist

- [ ] Add `_BridgeResult` class to `bootstrap.py`
- [ ] Add `_drive_coroutine` to `bootstrap.py`
- [ ] Add `_maybe_sync` to `bootstrap.py`
- [ ] Apply `_maybe_sync` in `_serialize_one` for callable args
- [ ] Return `_BridgeResult` from `_bridge_call` instead of raw value
- [ ] Add `__aiter__` stub with clear error to `_Proxy`
- [ ] Move `asyncio` and `concurrent.futures` out of safety checker deny list
- [ ] Update deny list error messages: "threading not available in WASM sandbox"
- [ ] Add callback depth protection to `_register_callback`
- [ ] Test: `asyncio.run()` with numpy calls
- [ ] Test: `await np.dot(a, b)` in async context
- [ ] Test: `scipy.optimize.minimize` with `async def` objective
- [ ] Test: nested coroutines via `asyncio.gather`
- [ ] Phase 2: `__aiter__`/`__anext__` on `_Proxy` for pandas iteration
