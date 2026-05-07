#!/usr/bin/env bash
# scripts/docker-test.sh
# End-to-end local test: build the Chrysalis image, start the server in a
# container, and hit /run with a battery of cases that exercise every
# documented path (pure Python, numpy bridge, ndarray return via shm,
# filter denial, safety pre-check, timeout).
#
# Usage:
#   bash scripts/docker-test.sh                run the full suite
#   PORT=9090 bash scripts/docker-test.sh      pick a different host port
#   KEEP_RUNNING=1 bash scripts/docker-test.sh leave container up for debug
#   REBUILD=0 bash scripts/docker-test.sh      skip docker build (use cached image)

set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE=${IMAGE:-chrysalis:test}
NAME=${NAME:-chrysalis-test-$$}
PORT=${PORT:-8080}
REBUILD=${REBUILD:-1}
KEEP_RUNNING=${KEEP_RUNNING:-0}

GREEN=$'\033[32m'
RED=$'\033[31m'
DIM=$'\033[2m'
RESET=$'\033[0m'

cleanup() {
    if [[ "$KEEP_RUNNING" == "1" ]]; then
        echo "${DIM}KEEP_RUNNING=1 — container $NAME left running on :$PORT${RESET}"
        echo "${DIM}  logs: docker logs -f $NAME${RESET}"
        echo "${DIM}  stop: docker rm -f $NAME${RESET}"
        return
    fi
    docker logs "$NAME" > "/tmp/${NAME}.log" 2>&1 || true
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    echo "${DIM}server logs saved to /tmp/${NAME}.log${RESET}"
}
trap cleanup EXIT

if [[ "$REBUILD" == "1" ]]; then
    echo "==> building $IMAGE"
    docker build -t "$IMAGE" .
fi

echo "==> starting container $NAME on :$PORT"
docker run -d --rm --name "$NAME" -p "$PORT:8080" "$IMAGE" >/dev/null

echo -n "==> waiting for /health "
for i in $(seq 1 60); do
    if curl -fs "http://localhost:$PORT/health" >/dev/null 2>&1; then
        echo "ready"
        break
    fi
    echo -n "."
    sleep 1
    if [[ $i -eq 60 ]]; then
        echo " TIMEOUT"
        echo "--- container logs ---"
        docker logs "$NAME" || true
        exit 1
    fi
done

PASS=0
FAIL=0
FAILED_NAMES=()

# run_case <name> <json_body> <grep_pattern>
#   PASS if the response body matches the pattern.
run_case() {
    local name="$1" body="$2" pattern="$3"
    local resp
    resp=$(curl -s --max-time 15 -X POST "http://localhost:$PORT/run" \
        -H 'Content-Type: application/json' \
        --data-raw "$body" || echo '{"status":"curl-error"}')
    if printf '%s' "$resp" | grep -Eq "$pattern"; then
        printf "  ${GREEN}PASS${RESET}  %s\n" "$name"
        PASS=$((PASS+1))
    else
        printf "  ${RED}FAIL${RESET}  %s\n        resp: %s\n" "$name" "$resp"
        FAIL=$((FAIL+1))
        FAILED_NAMES+=("$name")
    fi
}

echo "==> running test suite"

# ---------- pure Python (no bridge) ----------
run_case "pure python: sum(range(100))" \
    '{"code":"print(sum(range(100)))"}' \
    '"stdout":"4950'

run_case "pure python: arithmetic" \
    '{"code":"print(2**10)"}' \
    '"stdout":"1024'

# ---------- numpy bridge: scalar return ----------
run_case "numpy.dot 1-D float64" \
    '{"code":"import numpy as np\nprint(np.dot(np.array([1.,2.,3.]), np.array([4.,5.,6.])))"}' \
    '"stdout":"32(\.0)?'

run_case "numpy.sum 1-D" \
    '{"code":"import numpy as np\nprint(int(np.sum(np.arange(100))))"}' \
    '"stdout":"4950'

# ---------- numpy bridge: ndarray return (exercises shm path) ----------
run_case "numpy.linalg.solve 3x3 (round-trip)" \
    '{"code":"import numpy as np\nA=np.array([[3.,2.,-1.],[2.,-2.,4.],[-1.,0.5,-1.]])\nb=np.array([1.,-2.,0.])\nx=np.linalg.solve(A,b)\nprint(\"shape=\", x.shape)"}' \
    '"status":"ok"'

run_case "numpy.arange large (~1MB)" \
    '{"code":"import numpy as np\na=np.arange(125000, dtype=np.float64)\nprint(int(np.sum(a)))"}' \
    '"stdout":"7812437500'

# ---------- _TensorView universal ops (PR-1) ----------
# Exercise the package-agnostic flat-bytes view: indexing, iteration, tolist,
# multi-dim selection. No numpy-side support needed beyond the bytes-and-shape
# the worker already produces.
run_case "tensor: 1-D int indexing" \
    '{"code":"import numpy as np\nprint(np.arange(5)[3])"}' \
    '"stdout":"3'

run_case "tensor: iteration" \
    '{"code":"import numpy as np\nprint(sum(int(x) for x in np.arange(4)))"}' \
    '"stdout":"6'

run_case "tensor: 1-D slice + tolist" \
    '{"code":"import numpy as np\nprint(np.arange(5)[1:4].tolist())"}' \
    '"stdout":"\[1, 2, 3\]'

run_case "tensor: 2-D first-axis indexing" \
    '{"code":"import numpy as np\nprint(np.eye(3)[0].tolist())"}' \
    '"stdout":"\[1.0, 0.0, 0.0\]'

run_case "tensor: multi-dim tuple index" \
    '{"code":"import numpy as np\nprint(np.eye(3)[1,1])"}' \
    '"stdout":"1.0'

run_case "tensor: linalg.solve actual values" \
    '{"code":"import numpy as np\nA=np.array([[3.,2.],[1.,4.]]); b=np.array([7.,10.])\nx=np.linalg.solve(A,b)\nprint(round(float(x[0]),4), round(float(x[1]),4))"}' \
    '"stdout":"0.8 2.3'

run_case "tensor: large array short repr" \
    '{"code":"import numpy as np\nr=repr(np.arange(1000))\nprint(len(r) < 100, \"size=1000\" in r)"}' \
    '"stdout":"True True'

# ---------- scipy ----------
run_case "scipy.linalg.det 2x2" \
    '{"code":"import scipy.linalg as sl\nimport numpy as np\nprint(round(sl.det(np.array([[1.,2.],[3.,4.]])), 4))"}' \
    '"status":"ok"'

# ---------- filter enforcement ----------
run_case "filter blocks numpy.save" \
    '{"code":"import numpy as np\nnp.save(\"/tmp/x\", np.array([1.,2.,3.]))"}' \
    'not allowed by filter'

run_case "filter blocks numpy.load (autograder profile)" \
    '{"code":"import numpy as np\nnp.load(\"/tmp/x.npy\")"}' \
    'not allowed by filter'

# ---------- safety pre-check (regex) ----------
run_case "safety blocks os import" \
    '{"code":"import os\nos.system(\"ls\")"}' \
    '"status":"blocked"'

run_case "safety blocks eval" \
    '{"code":"eval(\"1+1\")"}' \
    '"status":"blocked"'

run_case "safety blocks open()" \
    '{"code":"open(\"/etc/passwd\").read()"}' \
    '"status":"blocked"'

# ---------- math-only profile ----------
run_case "math-only profile: numpy.sin works" \
    '{"code":"import numpy as np\nprint(round(float(np.sin(0.0)), 4))","filter":"math-only"}' \
    '"status":"ok"'

run_case "math-only profile: numpy.dot blocked" \
    '{"code":"import numpy as np\nprint(np.dot(np.array([1.,2.]), np.array([3.,4.])))","filter":"math-only"}' \
    'not allowed by filter'

# ---------- timeout ----------
# Uses a bridged call inside the loop so wazero gets WASI yield points to preempt.
# A tight pure-Python loop cannot currently be interrupted (known limitation).
run_case "timeout: bridge loop killed" \
    '{"code":"import numpy as np\nwhile True:\n    np.dot(np.array([1.,2.]), np.array([3.,4.]))","timeout_sec":2}' \
    '"status":"error"|"timing"'

# ---------- error surface ----------
run_case "user runtime error reported" \
    '{"code":"x = 1/0"}' \
    'ZeroDivisionError|"status":"error"'

# Sync-only scope: passing a callable to a bridged function must fail at the
# shim with a clear TypeError, not silently misbehave on the worker side.
run_case "callable args rejected (sync-only scope)" \
    '{"code":"import scipy.optimize as so\ndef f(x): return x*x\nso.brentq(f, 0, 10)"}' \
    'callable arguments to bridged functions are not supported'

# ---------- worker pool: concurrent requests don't serialise ----------
# Time a single request as a baseline, then issue 4 in parallel and compare.
# With 4 workers the parallel wall-clock should be ~1× the single time;
# if requests serialised on one worker it would be ~4×.
echo "  ${DIM}running concurrency test (single baseline, then 4 parallel)${RESET}"
SLOW_BODY='{"code":"import numpy as np\nfor i in range(2000):\n    np.dot(np.array([1.0,2.0]), np.array([3.0,4.0]))\nprint(\"done\")","timeout_sec":30}'
now_ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

# Single request baseline.
S0=$(now_ms)
curl -s --max-time 30 -X POST "http://localhost:$PORT/run" \
    -H 'Content-Type: application/json' --data-raw "$SLOW_BODY" \
    > "/tmp/conc_$$_baseline.out"
S1=$(now_ms)
SINGLE_MS=$((S1 - S0))
if grep -q '"status":"ok"' "/tmp/conc_$$_baseline.out"; then
    SINGLE_OK=true
else
    SINGLE_OK=false
    echo "  ${DIM}baseline body: $(head -c 200 /tmp/conc_$$_baseline.out)${RESET}"
fi
rm -f "/tmp/conc_$$_baseline.out"

# 4 parallel requests.
P0=$(now_ms)
for i in 1 2 3 4; do
    (curl -s --max-time 30 -X POST "http://localhost:$PORT/run" \
        -H 'Content-Type: application/json' \
        --data-raw "$SLOW_BODY" > "/tmp/conc_$$_${i}.out") &
done
wait
P1=$(now_ms)
PARALLEL_MS=$((P1 - P0))

PAR_OK=true
for i in 1 2 3 4; do
    if ! grep -q '"status":"ok"' "/tmp/conc_$$_${i}.out"; then
        PAR_OK=false
        echo "  ${DIM}parallel #$i body: $(head -c 200 /tmp/conc_$$_${i}.out)${RESET}"
    fi
    rm -f "/tmp/conc_$$_${i}.out"
done

# Parallel should be no worse than 2× single (4× would mean full serialisation).
PARALLEL_BUDGET=$((SINGLE_MS * 2))
if $SINGLE_OK && $PAR_OK && [[ $SINGLE_MS -gt 100 ]] && [[ $PARALLEL_MS -lt $PARALLEL_BUDGET ]]; then
    printf "  ${GREEN}PASS${RESET}  concurrency: single=%dms 4-parallel=%dms (budget %dms)\n" \
        "$SINGLE_MS" "$PARALLEL_MS" "$PARALLEL_BUDGET"
    PASS=$((PASS+1))
else
    printf "  ${RED}FAIL${RESET}  concurrency: single=%dms parallel=%dms budget=%dms ok=%s/%s\n" \
        "$SINGLE_MS" "$PARALLEL_MS" "$PARALLEL_BUDGET" "$SINGLE_OK" "$PAR_OK"
    FAIL=$((FAIL+1))
    FAILED_NAMES+=("concurrency: 4 parallel /run vs single baseline")
fi

echo ""
if [[ $FAIL -eq 0 ]]; then
    printf "==> ${GREEN}%d passed, 0 failed${RESET}\n" "$PASS"
    exit 0
else
    printf "==> ${GREEN}%d passed${RESET}, ${RED}%d failed${RESET}\n" "$PASS" "$FAIL"
    printf "    failed cases: %s\n" "${FAILED_NAMES[*]}"
    exit 1
fi
