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

# Run worker standalone for testing
test-worker:
	python3 -c "
import socket, struct, json, time
# Start worker in background
import subprocess, os, sys
sock = '/tmp/test_worker.sock'
try: os.remove(sock)
except: pass
ln = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
ln.bind(sock); ln.listen(1)
p = subprocess.Popen([sys.executable, 'shim/worker.py', sock])
conn, _ = ln.accept()
def send(msg):
    d = json.dumps(msg).encode()
    conn.sendall(struct.pack('>I', len(d)) + d)
def recv():
    h = conn.recv(4)
    n = struct.unpack('>I', h)[0]
    return json.loads(conn.recv(n))
print('ready:', recv())
send({'type':'ping'})
print('pong:', recv())
send({'id':'1','type':'probe','target':'numpy'})
print('probe numpy:', recv())
send({'id':'2','type':'call','func':'numpy.dot','args':[
    {'type':'ndarray_inline','b64':__import__('base64').b64encode(b'\\x00\\x00\\x80?\\x00\\x00\\x00@\\x00\\x00@@').decode(),'shape':[3],'dtype':'float32'},
    {'type':'ndarray_inline','b64':__import__('base64').b64encode(b'\\x00\\x00\\x80?\\x00\\x00\\x00@\\x00\\x00@@').decode(),'shape':[3],'dtype':'float32'},
],'kwargs':{}})
print('dot result:', recv())
send({'type':'shutdown'})
print('shutdown:', recv())
p.wait()
print('worker exited cleanly')
"

clean:
	rm -f $(BINARY)
	rm -f $(SOCK)
