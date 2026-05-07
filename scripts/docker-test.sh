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
# _NumpyLike (the WASM-side array proxy) is minimal — no .round / .tolist /
# arithmetic. We can only check that the bridge round-trips an array result
# without erroring; reading values out is up to the worker side.
run_case "numpy.linalg.solve 3x3 (round-trip)" \
    '{"code":"import numpy as np\nA=np.array([[3.,2.,-1.],[2.,-2.,4.],[-1.,0.5,-1.]])\nb=np.array([1.,-2.,0.])\nx=np.linalg.solve(A,b)\nprint(\"shape=\", x.shape)"}' \
    '"status":"ok"'

run_case "numpy.arange large (~1MB)" \
    '{"code":"import numpy as np\na=np.arange(125000, dtype=np.float64)\nprint(int(np.sum(a)))"}' \
    '"stdout":"7812437500'

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

echo ""
if [[ $FAIL -eq 0 ]]; then
    printf "==> ${GREEN}%d passed, 0 failed${RESET}\n" "$PASS"
    exit 0
else
    printf "==> ${GREEN}%d passed${RESET}, ${RED}%d failed${RESET}\n" "$PASS" "$FAIL"
    printf "    failed cases: %s\n" "${FAILED_NAMES[*]}"
    exit 1
fi
