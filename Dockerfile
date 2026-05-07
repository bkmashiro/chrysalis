# Single-stage image: Go toolchain + Python with numpy/scipy.
# Built for local end-to-end testing on a Linux host (or Docker Desktop on macOS).
FROM golang:1.22-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
        python3 \
        python3-numpy \
        python3-scipy \
        python3-pandas \
        curl \
        ca-certificates \
        make \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache Go module downloads in a layer that only invalidates when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Cache the CPython-WASI download (~14 MB) — only re-runs if Makefile changes.
COPY Makefile ./
RUN make python-wasm

# Build.
COPY . .
RUN go build -o bin/chrysalis ./cmd/chrysalis

EXPOSE 8080

CMD ["./bin/chrysalis", "serve", \
     "--wasm", "wasm/python.wasm", \
     "--filter-dir", "filters", \
     "--worker", "shim/worker.py", \
     "--shim", "shim/bootstrap.py", \
     "--sock", "/tmp/chrysalis_worker.sock", \
     "--port", "8080"]
