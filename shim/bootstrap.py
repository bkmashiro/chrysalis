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
import types
import importlib.machinery

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
    # Bridged-name proxy (e.g. np.float64 passed as dtype=, np.linalg passed as
    # a module reference). Pass by name; the worker resolves the live object.
    # Must come BEFORE the generic callable check — every _BridgedFn is callable.
    # Use object.__getattribute__ to avoid triggering _ProxyModule.__getattr__,
    # which would send a worker probe for "_name" / "_ns".
    if isinstance(obj, _BridgedFn):
        return {"type": "bridge_ref", "value": object.__getattribute__(obj, "_name")}
    if isinstance(obj, _ProxyModule):
        return {"type": "bridge_ref", "value": object.__getattribute__(obj, "_ns")}
    if callable(obj):
        # Sync-only scope: user-defined callables are not supported. Async/
        # coroutine bridging is a long-term direction (docs/design.md §8.7).
        # Fail fast with a clear message instead of registering a dead cb_id.
        raise TypeError(
            "Chrysalis bridge: callable arguments to bridged functions are "
            "not supported in sync mode (got %r). Inline the computation in "
            "user code, or split it across multiple /run requests."
            % type(obj).__name__
        )
    return {"type": "scalar", "value": str(obj)}


def _ser_array(arr):
    """
    Serialise an ndarray for the bridge.

    WASM cannot allocate /dev/shm files, so the bytes are inlined as base64;
    the Go dispatcher unpacks them into shm before forwarding to the worker.
    """
    import base64
    data_b64 = base64.b64encode(arr.tobytes()).decode("ascii")
    return {
        "type":   "ndarray",
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
        return _deser_tensor(obj)
    return obj.get("value")


def _deser_tensor(obj):
    """Reconstruct a tensor view from inline base64 bytes.

    WASM cannot read /dev/shm, so the host always inlines the raw bytes
    in the bridge response (the Go side keeps a shm handle separately for
    the case where the array is later re-passed through the bridge).
    """
    import base64
    b64 = obj.get("b64", "")
    raw = base64.b64decode(b64) if b64 else b""
    shape = obj.get("shape", [])
    dtype = obj.get("dtype", "float64")
    return _TensorView(raw, shape, dtype)


# ---------------------------------------------------------------------------
# _TensorView — universal flat-bytes view over a worker-side array
# ---------------------------------------------------------------------------
# Knows nothing about numpy / torch / pyarrow — only (shape, dtype, raw bytes).
# Operations that fit in WASM (indexing, iteration, tolist, repr) are local;
# anything heavier (multi-dim slicing, arithmetic, reductions) is left to a
# future re-bridge mechanism (PR-2).

import struct as _struct

# Native-byte-order format chars + itemsize. Matches numpy's str(dtype) for
# native-endian arrays; non-native byte order is rejected with a TypeError.
_DTYPE_FMT = {
    "float64": ("d", 8),
    "float32": ("f", 4),
    "int64":   ("q", 8),
    "int32":   ("i", 4),
    "int16":   ("h", 2),
    "int8":    ("b", 1),
    "uint64":  ("Q", 8),
    "uint32":  ("I", 4),
    "uint16":  ("H", 2),
    "uint8":   ("B", 1),
    "bool":    ("?", 1),
}


class _TensorView:
    """Flat-bytes view over a worker-side array.

    Universal across array libraries — built from (shape, dtype, raw bytes)
    only. The wire protocol decomposes any sufficiently-array-like result to
    this form, so numpy.ndarray, torch.Tensor, pyarrow.Array, etc. all map
    here without library-specific shim code.
    """

    def __init__(self, data, shape, dtype):
        self._data    = bytes(data)
        self.shape    = tuple(shape)
        self.dtype    = str(dtype)
        self.ndim     = len(self.shape)
        self.size     = 1
        for d in self.shape:
            self.size *= d
        self.nbytes   = len(self._data)
        fmt_size = _DTYPE_FMT.get(self.dtype)
        if fmt_size is None:
            self._fmt, self._itemsize = None, 0
        else:
            self._fmt, self._itemsize = fmt_size

    # ── Decoding helpers ─────────────────────────────────────────────────
    def _require_decodable(self):
        if self._fmt is None:
            raise TypeError(
                "Chrysalis _TensorView: unsupported dtype %r "
                "(only native-endian fixed-width dtypes are decodable in WASM)"
                % self.dtype
            )

    def _decode_at(self, flat_idx):
        self._require_decodable()
        off = flat_idx * self._itemsize
        return _struct.unpack_from(self._fmt, self._data, off)[0]

    def _slice_bytes(self, byte_start, byte_stop):
        return self._data[byte_start:byte_stop]

    # ── Container protocol ───────────────────────────────────────────────
    def __len__(self):
        if self.ndim == 0:
            raise TypeError("len() of 0-d _TensorView")
        return self.shape[0]

    def __getitem__(self, idx):
        # Single int — first-axis selection.
        if isinstance(idx, int):
            if self.ndim == 0:
                raise IndexError("invalid index to 0-d _TensorView")
            n = self.shape[0]
            if idx < 0:
                idx += n
            if idx < 0 or idx >= n:
                raise IndexError(idx)
            if self.ndim == 1:
                return self._decode_at(idx)
            # Sub-view along leading axis.
            self._require_decodable()
            tail_size = self.size // n
            tail_bytes = tail_size * self._itemsize
            start = idx * tail_bytes
            return _TensorView(self._data[start:start + tail_bytes],
                               self.shape[1:], self.dtype)

        # Slice — 1-D only for now.
        if isinstance(idx, slice):
            if self.ndim != 1:
                raise NotImplementedError(
                    "_TensorView: multi-dim slicing is not implemented; "
                    "future PR will route to the worker via re-bridge"
                )
            self._require_decodable()
            start, stop, step = idx.indices(self.shape[0])
            if step == 1:
                return _TensorView(
                    self._data[start * self._itemsize:stop * self._itemsize],
                    (max(stop - start, 0),), self.dtype,
                )
            pieces = []
            for i in range(start, stop, step):
                pieces.append(self._data[i * self._itemsize:(i + 1) * self._itemsize])
            return _TensorView(b"".join(pieces), (len(pieces),), self.dtype)

        # All-int tuple — multi-dim flat-offset (row-major).
        if isinstance(idx, tuple):
            if not all(isinstance(x, int) for x in idx):
                raise NotImplementedError(
                    "_TensorView: only all-int tuple indexing is implemented"
                )
            if len(idx) > self.ndim:
                raise IndexError("too many indices")
            if len(idx) < self.ndim:
                # Partial tuple — same as recursive [i0][i1]...
                view = self
                for i in idx:
                    view = view[i]
                return view
            # Full index — flat offset.
            flat = 0
            stride = 1
            for size, i in zip(reversed(self.shape), reversed(idx)):
                if i < 0:
                    i += size
                if i < 0 or i >= size:
                    raise IndexError(idx)
                flat += i * stride
                stride *= size
            return self._decode_at(flat)

        raise TypeError("_TensorView: unsupported index type %r"
                        % type(idx).__name__)

    def __iter__(self):
        if self.ndim == 0:
            yield self._decode_at(0)
            return
        for i in range(self.shape[0]):
            yield self[i]

    def __contains__(self, value):
        for x in self:
            if x == value:
                return True
        return False

    # ── Conversions ──────────────────────────────────────────────────────
    def tolist(self):
        if self.ndim == 0:
            return self._decode_at(0)
        if self.ndim == 1:
            return [self._decode_at(i) for i in range(self.shape[0])]
        return [self[i].tolist() for i in range(self.shape[0])]

    def tobytes(self):
        return self._data

    # ── Scalar conversions for size-1 views ──────────────────────────────
    def __float__(self):
        if self.size != 1:
            raise TypeError("only size-1 _TensorView can be converted to float")
        return float(self._decode_at(0))

    def __int__(self):
        if self.size != 1:
            raise TypeError("only size-1 _TensorView can be converted to int")
        return int(self._decode_at(0))

    def __bool__(self):
        if self.size != 1:
            raise ValueError(
                "truth value of multi-element _TensorView is ambiguous"
            )
        return bool(self._decode_at(0))

    def __index__(self):
        if self.size != 1 or "int" not in self.dtype:
            raise TypeError("only size-1 integer _TensorView is index-able")
        return int(self._decode_at(0))

    # ── Repr / str ───────────────────────────────────────────────────────
    def __repr__(self):
        if self.ndim == 0:
            try:
                return "TensorView(%r, dtype=%r)" % (self._decode_at(0), self.dtype)
            except Exception:
                return "TensorView(<undecodable>, dtype=%r)" % self.dtype
        if self.size <= 12 and self._fmt is not None:
            try:
                return "TensorView(%r, dtype=%r)" % (self.tolist(), self.dtype)
            except Exception:
                pass
        return "TensorView(shape=%r, dtype=%r, size=%d)" % (
            self.shape, self.dtype, self.size,
        )

    def __str__(self):
        # Make 0-d views print like the bare value (e.g. np.dot scalar result).
        if self.ndim == 0 and self._fmt is not None:
            try:
                return str(self._decode_at(0))
            except Exception:
                pass
        if self.ndim == 1 and self.size <= 12 and self._fmt is not None:
            try:
                return str(self.tolist())
            except Exception:
                pass
        return repr(self)

    def __format__(self, spec):
        return format(self.__str__(), spec)

    # ── Equality / hashing ───────────────────────────────────────────────
    def __eq__(self, other):
        if isinstance(other, _TensorView):
            return (self.shape == other.shape
                    and self.dtype == other.dtype
                    and self._data == other._data)
        return NotImplemented

    def __hash__(self):
        return hash((self.shape, self.dtype, self._data))


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
    # PEP 451: Python 3.12 dropped the legacy find_module/load_module hooks.
    def find_spec(self, name, path=None, target=None):
        top = name.split(".")[0]
        if top in _BRIDGED_TOPS:
            return importlib.machinery.ModuleSpec(
                name, _BridgeLoader(name), is_package=True,
            )
        return None


class _BridgeLoader:
    def __init__(self, name):
        self.name = name

    def create_module(self, spec):
        return _ProxyModule(spec.name)

    def exec_module(self, module):
        pass


class _ProxyModule(types.ModuleType):
    """Dynamic proxy: attribute access → bridge probe; call → bridge call."""

    def __init__(self, ns: str):
        super().__init__(ns)
        object.__setattr__(self, "_ns", ns)
        object.__setattr__(self, "_attr_cache", {})
        # Mark as package so the import system can resolve submodules.
        self.__path__ = []

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

        # result may be a dict (probe response) or a _TensorView (shouldn't happen).
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
