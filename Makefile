.PHONY: deps build run test-local python-wasm clean

BINARY   := bin/chrysalis
WASM     := wasm/python.wasm
WORKER   := shim/worker.py
SHIM     := shim/bootstrap.py
FILTERS  := filters
PORT     := 8080
SOCK     := /tmp/chrysalis_worker.sock

deps:
	go mod tidy

build: deps
	mkdir -p bin
	go build -o $(BINARY) ./cmd/chrysalis

# Download CPython-WASI binary (run once, ~14 MB)
python-wasm:
	mkdir -p wasm
	curl -fL \
		"https://github.com/vmware-labs/webassembly-language-runtimes/releases/download/python%2F3.12.0%2B20231211-040d5a6/python-3.12.0.wasm" \
		-o $(WASM)
	@echo "Downloaded $(WASM)"

run: build
	./$(BINARY) serve \
		--wasm    $(WASM) \
		--filter-dir $(FILTERS) \
		--worker  $(WORKER) \
		--shim    $(SHIM) \
		--sock    $(SOCK) \
		--port    $(PORT)

# Quick smoke test — requires server to be running
test-local:
	@echo "=== numpy.dot test ==="
	curl -s -X POST http://localhost:$(PORT)/run \
		-H 'Content-Type: application/json' \
		-d '{"code":"import numpy as np\nresult = np.dot(np.array([1.0,2.0,3.0]), np.array([4.0,5.0,6.0]))\nprint(result)","filter":"autograder","timeout_sec":30}' \
		| python3 -m json.tool
	@echo ""
	@echo "=== pure Python test ==="
	curl -s -X POST http://localhost:$(PORT)/run \
		-H 'Content-Type: application/json' \
		-d '{"code":"x = sum(range(100))\nprint(x)","filter":"autograder","timeout_sec":30}' \
		| python3 -m json.tool
	@echo ""
	@echo "=== safety block test ==="
	curl -s -X POST http://localhost:$(PORT)/run \
		-H 'Content-Type: application/json' \
		-d '{"code":"import os\nos.system(\"ls\")","filter":"autograder","timeout_sec":30}' \
		| python3 -m json.tool

# End-to-end Docker test (build image, run server, exercise /run with curl).
docker-test:
	bash scripts/docker-test.sh

clean:
	rm -f $(BINARY)
	rm -f $(SOCK)
