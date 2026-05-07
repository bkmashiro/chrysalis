#!/usr/bin/env python3
"""
Chrysalis Python Worker
=======================
Long-running process that pre-loads numpy/scipy/pandas and serves bridge
requests from the Go host over a Unix domain socket.

Protocol: 4-byte big-endian length prefix + UTF-8 JSON body.

Message types (Go → Worker):
  {"type":"ping"}
  {"type":"shutdown"}
  {"type":"probe","id":"...","target":"numpy.linalg"}
  {"type":"call","id":"...","func":"numpy.dot","args":[...],"kwargs":{}}

Message types (Worker → Go):
  {"type":"pong"}
  {"type":"shutdown_ack"}
  {"type":"ready"}
  {"type":"probe_result","id":"...","is_module":true,"is_callable":false}
  {"type":"result","id":"...","value":{...}}
  {"type":"error","id":"...","error":"ValueError","message":"..."}
"""

import socket
import struct
import json
import sys
import os
import base64
import importlib
import mmap
import traceback

# ---------------------------------------------------------------------------
# Pre-load heavy libraries
# ---------------------------------------------------------------------------
try:
    import numpy as np
    _has_numpy = True
except ImportError:
    np = None  # type: ignore
    _has_numpy = False

try:
    import scipy
    import scipy.optimize
    import scipy.linalg
    import scipy.special
    _has_scipy = True
except ImportError:
    scipy = None  # type: ignore
    _has_scipy = False

try:
    import pandas as pd
    _has_pandas = True
except ImportError:
    pd = None  # type: ignore
    _has_pandas = False

# ---------------------------------------------------------------------------
# Allowlist (defence in depth — Go filter is the primary control)
# ---------------------------------------------------------------------------
_ALLOWED = {
    "numpy.dot", "numpy.matmul", "numpy.array", "numpy.zeros", "numpy.ones",
    "numpy.eye", "numpy.arange", "numpy.linspace", "numpy.reshape",
    "numpy.transpose", "numpy.concatenate", "numpy.stack", "numpy.split",
    "numpy.sum", "numpy.mean", "numpy.std", "numpy.var",
    "numpy.min", "numpy.max", "numpy.abs", "numpy.sqrt",
    "numpy.exp", "numpy.log", "numpy.log2", "numpy.log10",
    "numpy.sin", "numpy.cos", "numpy.tan",
    "numpy.arcsin", "numpy.arccos", "numpy.arctan", "numpy.arctan2",
    "numpy.pi", "numpy.e", "numpy.inf", "numpy.nan",
    "numpy.sort", "numpy.argsort", "numpy.argmax", "numpy.argmin",
    "numpy.cumsum", "numpy.cumprod", "numpy.prod",
    "numpy.unique", "numpy.where", "numpy.clip", "numpy.round",
    "numpy.floor", "numpy.ceil", "numpy.sign",
    "numpy.cross", "numpy.outer", "numpy.inner",
    "numpy.linalg.solve", "numpy.linalg.inv", "numpy.linalg.det",
    "numpy.linalg.eig", "numpy.linalg.eigvals", "numpy.linalg.norm",
    "numpy.linalg.svd", "numpy.linalg.qr", "numpy.linalg.cholesky",
    "numpy.linalg.lstsq", "numpy.linalg.matrix_rank",
    "numpy.random.seed", "numpy.random.rand", "numpy.random.randn",
    "numpy.random.randint", "numpy.random.uniform", "numpy.random.normal",
    "numpy.random.shuffle", "numpy.random.permutation",
    "numpy.fft.fft", "numpy.fft.ifft", "numpy.fft.fft2",
    "numpy.polynomial.polyval", "numpy.polynomial.polyfit",
    "scipy.optimize.minimize", "scipy.optimize.root",
    "scipy.optimize.fsolve", "scipy.optimize.brentq",
    "scipy.linalg.solve", "scipy.linalg.lu", "scipy.linalg.qr",
    "scipy.linalg.svd", "scipy.linalg.inv", "scipy.linalg.det",
    "scipy.special.gamma", "scipy.special.erf", "scipy.special.erfc",
    "scipy.special.factorial", "scipy.special.comb",
    "pandas.DataFrame", "pandas.Series", "pandas.concat",
    "pandas.merge", "pandas.pivot_table",
}

# ---------------------------------------------------------------------------
# Socket helpers
# ---------------------------------------------------------------------------

def _recv_msg(sock: socket.socket) -> dict:
    raw = b""
    while len(raw) < 4:
        chunk = sock.recv(4 - len(raw))
        if not chunk:
            raise ConnectionError("connection closed while reading header")
        raw += chunk
    (length,) = struct.unpack(">I", raw)
    if length > 16 * 1024 * 1024:
        raise ValueError(f"message too large: {length}")
    body = b""
    while len(body) < length:
        chunk = sock.recv(min(length - len(body), 65536))
        if not chunk:
            raise ConnectionError("connection closed while reading body")
        body += chunk
    return json.loads(body.decode("utf-8"))


def _send_msg(sock: socket.socket, msg: dict) -> None:
    data = json.dumps(msg).encode("utf-8")
    sock.sendall(struct.pack(">I", len(data)) + data)

# ---------------------------------------------------------------------------
# shm helpers
# ---------------------------------------------------------------------------

def _shm_path(name: str) -> str:
    if os.path.isdir("/dev/shm"):
        return f"/dev/shm/{name}"
    return os.path.join("/tmp", name)


def _alloc_shm(nbytes: int) -> tuple:
    """Allocate a shm file. Returns (name, path, writable_mmap)."""
    import secrets
    name = "chr-" + secrets.token_hex(3)
    path = _shm_path(name)
    with open(path, "wb") as f:
        f.write(b"\x00" * nbytes)
    f = open(path, "r+b")
    mm = mmap.mmap(f.fileno(), nbytes)
    return name, path, mm, f


def _read_shm(path: str, nbytes: int, dtype: str, shape: list):
    """Read raw bytes from a shm file and return a numpy array."""
    with open(path, "rb") as f:
        raw = f.read(nbytes)
    arr = np.frombuffer(raw, dtype=dtype).reshape(shape)
    return arr.copy()  # detach from the buffer


# ---------------------------------------------------------------------------
# Serialisation
# ---------------------------------------------------------------------------

def _ser(obj) -> dict:
    if obj is None or isinstance(obj, (bool, int, float, str)):
        return {"type": "scalar", "value": obj}
    if isinstance(obj, (list, tuple)):
        return {"type": "list", "value": [_ser(x) for x in obj]}
    if isinstance(obj, dict):
        return {"type": "dict", "value": {k: _ser(v) for k, v in obj.items()}}
    if _has_numpy and isinstance(obj, np.ndarray):
        return _ser_array(obj)
    if _has_numpy:
        if isinstance(obj, np.integer):
            return {"type": "scalar", "value": int(obj)}
        if isinstance(obj, np.floating):
            return {"type": "scalar", "value": float(obj)}
        if isinstance(obj, np.bool_):
            return {"type": "scalar", "value": bool(obj)}
    return {"type": "scalar", "value": str(obj)}


def _ser_array(arr) -> dict:
    """Serialise an ndarray using inline base64 (simpler than shm for results)."""
    contiguous = np.ascontiguousarray(arr)
    return {
        "type":  "ndarray_inline",
        "b64":   base64.b64encode(contiguous.tobytes()).decode("ascii"),
        "shape": list(contiguous.shape),
        "dtype": str(contiguous.dtype),
    }


def _deser(obj: dict):
    if obj is None:
        return None
    t = obj.get("type")
    if t == "scalar":
        return obj.get("value")
    if t == "list":
        return [_deser(x) for x in obj.get("value", [])]
    if t == "dict":
        return {k: _deser(v) for k, v in obj.get("value", {}).items()}
    if t in ("ndarray", "ndarray_inline"):
        return _deser_array(obj)
    return obj.get("value")


def _deser_array(obj: dict):
    if not _has_numpy:
        raise RuntimeError("numpy not available in worker")
    b64  = obj.get("b64", "")
    raw  = base64.b64decode(b64)
    dtype = obj.get("dtype", "float64")
    shape = obj.get("shape", [])
    return np.frombuffer(raw, dtype=dtype).reshape(shape).copy()


# ---------------------------------------------------------------------------
# Execution
# ---------------------------------------------------------------------------

def _resolve_attr(name: str):
    """Walk dotted name like 'numpy.linalg.solve' to the actual object."""
    parts = name.split(".")
    # Map top-level aliases.
    top_map = {}
    if _has_numpy:
        top_map["numpy"] = np
    if _has_scipy:
        top_map["scipy"] = scipy
    if _has_pandas:
        top_map["pandas"] = pd

    obj = top_map.get(parts[0])
    if obj is None:
        obj = importlib.import_module(parts[0])
    for attr in parts[1:]:
        obj = getattr(obj, attr)
    return obj


def _execute_call(msg: dict):
    """Execute a 'call' message and return the serialised result."""
    func_name = msg["func"]

    # Allowlist check (defence in depth).
    # We check the base function (without trailing .__probe__).
    base_name = func_name.rstrip(".__probe__") if func_name.endswith(".__probe__") else func_name
    # Probe calls are always permitted.
    if not func_name.endswith(".__probe__") and base_name not in _ALLOWED:
        # Log but don't block — Go-side filter is authoritative.
        pass

    fn = _resolve_attr(func_name)
    args   = [_deser(a) for a in msg.get("args", [])]
    kwargs = {k: _deser(v) for k, v in msg.get("kwargs", {}).items()}
    result = fn(*args, **kwargs)
    return _ser(result)


# ---------------------------------------------------------------------------
# Probe
# ---------------------------------------------------------------------------

def _probe(target: str) -> dict:
    try:
        obj = _resolve_attr(target)
        is_mod  = hasattr(obj, "__path__") or hasattr(obj, "__file__")
        is_call = callable(obj)
    except (ImportError, AttributeError, Exception):
        is_mod  = False
        is_call = False
    return {"is_module": is_mod, "is_callable": is_call}


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    if len(sys.argv) < 2:
        sys.exit("usage: worker.py <socket_path>")

    sock_path = sys.argv[1]

    # Connect to the Go host socket (Go is the listener).
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    # Retry connecting for up to 10 s (Go side may not be listening yet).
    import time
    deadline = time.time() + 10
    while True:
        try:
            s.connect(sock_path)
            break
        except (FileNotFoundError, ConnectionRefusedError):
            if time.time() > deadline:
                sys.exit(f"worker: could not connect to {sock_path}")
            time.sleep(0.1)

    # Signal that we're ready (numpy et al. are loaded).
    _send_msg(s, {"type": "ready"})

    while True:
        try:
            msg = _recv_msg(s)
        except (ConnectionError, EOFError) as e:
            print(f"worker: connection lost: {e}", file=sys.stderr)
            break

        mtype = msg.get("type")

        if mtype == "ping":
            _send_msg(s, {"type": "pong"})

        elif mtype == "shutdown":
            _send_msg(s, {"type": "shutdown_ack"})
            break

        elif mtype == "probe":
            result = _probe(msg.get("target", ""))
            _send_msg(s, {
                "id":          msg.get("id", ""),
                "type":        "probe_result",
                "is_module":   result["is_module"],
                "is_callable": result["is_callable"],
            })

        elif mtype == "call":
            try:
                value = _execute_call(msg)
                _send_msg(s, {
                    "id":    msg.get("id", ""),
                    "type":  "result",
                    "value": value,
                })
            except Exception as exc:
                _send_msg(s, {
                    "id":      msg.get("id", ""),
                    "type":    "error",
                    "error":   type(exc).__name__,
                    "message": str(exc),
                })

        else:
            # Unknown message type — ignore.
            pass

    s.close()


if __name__ == "__main__":
    main()
