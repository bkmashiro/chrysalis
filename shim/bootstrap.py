"""
Chrysalis bootstrap shim — injected into CPython-WASI before user code runs.

Communication with the Go host uses the module's stdin/stdout:
  • Python writes bridge requests  to stdout (length-prefixed JSON)
  • Python reads  bridge responses from stdin (length-prefixed JSON)

User print() output is captured in _user_stdout and flushed at the end via
a special {"__stdout__": "..."} sentinel message.

The shim installs a PEP 302 meta path finder so that:
    import numpy as np
    np.dot(a, b)
transparently routes through the Go ↔ Python-worker bridge.
"""

import sys
import io
import json
import struct

# ---------------------------------------------------------------------------
# Capture user stdout
# ---------------------------------------------------------------------------
_user_stdout = io.StringIO()
_orig_stdout = sys.stdout
sys.stdout = _user_stdout  # user print() goes here

# ---------------------------------------------------------------------------
# Bridge I/O  (stdin = responses from Go; stdout = requests to Go)
# ---------------------------------------------------------------------------
_in  = _orig_stdout  # repurposed: this is where we WRITE requests
_out = sys.stdin     # repurposed: this is where we READ responses

# Note: In CPython-WASI the file objects wrap the underlying fd 0/1 directly.
# We use the .buffer attribute for raw byte I/O.


def _bridge_write(data: bytes) -> None:
    """Write a length-prefixed frame to the host (via stdout fd)."""
    frame = struct.pack(">I", len(data)) + data
    try:
        _in.buffer.write(frame)
        _in.buffer.flush()
    except AttributeError:
        # Fallback for environments where .buffer is unavailable.
        import os
        os.write(1, frame)


def _bridge_read() -> bytes:
    """Read a length-prefixed frame from the host (via stdin fd)."""
    try:
        raw = _out.buffer.read(4)
    except AttributeError:
        import os
        raw = os.read(0, 4)
    if not raw or len(raw) < 4:
        raise EOFError("bridge closed")
    (n,) = struct.unpack(">I", raw)
    chunks = []
    remaining = n
    while remaining > 0:
        try:
            chunk = _out.buffer.read(remaining)
        except AttributeError:
            import os
            chunk = os.read(0, remaining)
        if not chunk:
            raise EOFError("bridge closed during read")
        chunks.append(chunk)
        remaining -= len(chunk)
    return b"".join(chunks)


def __shimmy_call__(request_json: str) -> str:
    """Send a bridge request and return the raw response JSON string."""
    _bridge_write(request_json.encode("utf-8"))
    return _bridge_read().decode("utf-8")


# ---------------------------------------------------------------------------
# Serialisation helpers
# ---------------------------------------------------------------------------

def _ser_one(obj):
    """Serialise a single Python value to a wire Arg dict."""
    if obj is None or isinstance(obj, (bool, int, float, str)):
        return {"type": "scalar", "value": obj}
    if isinstance(obj, (list, tuple)):
        return {"type": "list", "value": [_ser_one(x) for x in obj]}
    if isinstance(obj, dict):
        return {"type": "dict", "value": {k: _ser_one(v) for k, v in obj.items()}}
    # numpy-array-like: has shape, dtype, tobytes
    if hasattr(obj, "shape") and hasattr(obj, "dtype") and hasattr(obj, "tobytes"):
        return _ser_array(obj)
    if callable(obj):
        cb_id = _register_callback(obj)
        return {"type": "callback", "cb_id": cb_id}
    return {"type": "scalar", "value": str(obj)}


def _ser_array(arr):
    """
    Serialise a numpy array.

    We send the raw bytes as a base64-encoded scalar for simplicity in Phase 1.
    The Go host writes the bytes to shm and sends back a handle.
    """
    import base64
    data_b64 = base64.b64encode(arr.tobytes()).decode("ascii")
    return {
        "type":   "ndarray_inline",
        "b64":    data_b64,
        "shape":  list(arr.shape),
        "dtype":  str(arr.dtype),
    }


def _deser_one(obj):
    """Deserialise a wire Arg dict back to a Python value."""
    if obj is None:
        return None
    t = obj.get("type")
    if t == "scalar":
        return obj.get("value")
    if t == "list":
        return [_deser_one(x) for x in obj.get("value", [])]
    if t == "dict":
        return {k: _deser_one(v) for k, v in obj.get("value", {}).items()}
    if t == "ndarray":
        return _deser_ndarray(obj)
    if t == "ndarray_inline":
        return _deser_ndarray_inline(obj)
    return obj.get("value")


def _deser_ndarray(obj):
    """
    Reconstruct a numpy array from a shm handle.
    For Phase 1 the host returns ndarray_inline; this handles the shm case.
    """
    import base64, ctypes
    b64 = obj.get("b64", "")
    raw = base64.b64decode(b64) if b64 else b""
    shape = obj.get("shape", [])
    dtype = obj.get("dtype", "float64")
    # Reconstruct without importing numpy (we ARE the numpy proxy).
    # Use a bytearray + the _RawArray wrapper.
    return _NumpyLike(raw, shape, dtype)


def _deser_ndarray_inline(obj):
    """Reconstruct from inline base64 bytes."""
    return _deser_ndarray(obj)


class _NumpyLike:
    """Minimal array wrapper returned when the real numpy is not available."""
    def __init__(self, data: bytes, shape, dtype):
        self._data  = bytearray(data)
        self.shape  = tuple(shape)
        self.dtype  = dtype
        self.nbytes = len(data)

    def tobytes(self):
        return bytes(self._data)

    def __repr__(self):
        return f"<NumpyLike shape={self.shape} dtype={self.dtype}>"

    # Allow float conversion for scalar results.
    def __float__(self):
        import struct as _s
        fmt = {"float64": "d", "float32": "f", "int32": "i", "int64": "q"}.get(self.dtype, "d")
        return _s.unpack(fmt, self._data[:_s.calcsize(fmt)])[0]

    def __int__(self):
        return int(float(self))

    def __str__(self):
        if len(self._data) <= 8:
            try:
                return str(float(self))
            except Exception:
                pass
        return repr(self)

    def __format__(self, spec):
        return format(str(self), spec)


# ---------------------------------------------------------------------------
# Callback registry
# ---------------------------------------------------------------------------
_callbacks = {}
_cb_seq    = 0

def _register_callback(fn):
    global _cb_seq
    cid = _cb_seq
    _cb_seq += 1
    _callbacks[cid] = fn
    return cid


# ---------------------------------------------------------------------------
# Bridge call
# ---------------------------------------------------------------------------

def _bridge(fn: str, args: tuple, kwargs: dict):
    """Invoke fn via the Go bridge and return the deserialised result."""
    req = {
        "fn":     fn,
        "args":   [_ser_one(a) for a in args],
        "kwargs": {k: _ser_one(v) for k, v in kwargs.items()},
    }
    resp_json = __shimmy_call__(json.dumps(req))
    resp = json.loads(resp_json)
    if resp.get("error"):
        raise RuntimeError(f"Bridge error calling {fn!r}: {resp['error']}")
    value = resp.get("value")
    if value is None:
        # Probe response comes back as a scalar dict.
        return resp
    return _deser_one(value)


# ---------------------------------------------------------------------------
# PEP 302 meta-path finder
# ---------------------------------------------------------------------------

_BRIDGED_TOPS = frozenset(["numpy", "scipy", "pandas"])


class _BridgeFinder:
    def find_module(self, name, path=None):
        top = name.split(".")[0]
        if top in _BRIDGED_TOPS:
            return _BridgeLoader(name)
        return None


class _BridgeLoader:
    def __init__(self, name):
        self.name = name

    def load_module(self, name):
        if name in sys.modules:
            return sys.modules[name]
        mod = _ProxyModule(name)
        mod.__loader__  = self
        mod.__package__ = name
        mod.__path__    = []          # marks it as a package
        mod.__spec__    = None
        sys.modules[name] = mod
        return mod


class _ProxyModule:
    """Dynamic proxy: attribute access → bridge probe; call → bridge call."""

    def __init__(self, ns: str):
        object.__setattr__(self, "_ns", ns)
        object.__setattr__(self, "_attr_cache", {})

    def __getattr__(self, attr):
        if attr.startswith("__") and attr.endswith("__"):
            raise AttributeError(attr)
        cache = object.__getattribute__(self, "_attr_cache")
        if attr in cache:
            return cache[attr]
        ns  = object.__getattribute__(self, "_ns")
        full = f"{ns}.{attr}"

        # Probe the worker to find out what this is.
        try:
            result = _bridge(f"{full}.__probe__", (), {})
        except Exception:
            result = {}

        # result may be a dict (probe response) or a _NumpyLike (shouldn't happen).
        if isinstance(result, dict):
            is_mod  = result.get("is_module",   False)
            is_call = result.get("is_callable", False)
        else:
            is_mod  = False
            is_call = True   # assume callable for unknown probes

        if is_mod:
            sub = _ProxyModule(full)
            sub.__loader__  = None
            sub.__package__ = full
            sub.__path__    = []
            sub.__spec__    = None
            sys.modules[full] = sub
            cache[attr] = sub
            return sub

        if is_call:
            fn = _BridgedFn(full)
            cache[attr] = fn
            return fn

        # Attribute value (e.g. np.pi).
        val = _bridge(full, (), {})
        cache[attr] = val
        return val

    def __repr__(self):
        return f"<bridged module '{object.__getattribute__(self, '_ns')}'>"


class _BridgedFn:
    """Callable that forwards invocations through the bridge."""

    def __init__(self, name: str):
        self._name = name

    def __call__(self, *args, **kwargs):
        return _bridge(self._name, args, kwargs)

    def __repr__(self):
        return f"<bridged function '{self._name}'>"


# Install the meta-path finder.
sys.meta_path.insert(0, _BridgeFinder())


# ---------------------------------------------------------------------------
# Flush user stdout at exit  (registered via atexit)
# ---------------------------------------------------------------------------
import atexit as _atexit

def _flush_stdout():
    captured = _user_stdout.getvalue()
    if captured:
        sentinel = json.dumps({"__stdout__": captured}).encode("utf-8")
        _bridge_write(sentinel)
        # Read and discard the ACK so the bridge goroutine can proceed.
        try:
            _bridge_read()
        except Exception:
            pass

_atexit.register(_flush_stdout)
