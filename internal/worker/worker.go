// Package worker manages the long-running Python worker subprocess.
package worker

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Worker manages a single Python worker subprocess and its Unix-socket connection.
type Worker struct {
	sockPath   string
	workerPath string // path to worker.py
	cmd        *exec.Cmd
	conn       net.Conn
	mu         sync.Mutex // serialises Send/Recv pairs
	healthy    atomic.Bool
	reqSeq     atomic.Uint64
}

// Start launches the worker subprocess and waits for the "ready" handshake.
func Start(sockPath, workerScriptPath string) (*Worker, error) {
	// Remove stale socket.
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen on worker socket: %w", err)
	}
	defer ln.Close()

	cmd := exec.Command("python3", "-u", workerScriptPath, sockPath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start python worker: %w", err)
	}

	// Accept with timeout.
	_ = ln.(*net.UnixListener).SetDeadline(time.Now().Add(30 * time.Second))
	conn, err := ln.Accept()
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("worker did not connect: %w", err)
	}

	w := &Worker{
		sockPath:   sockPath,
		workerPath: workerScriptPath,
		cmd:        cmd,
		conn:       conn,
	}

	// Expect "ready" message.
	msg, err := w.recv()
	if err != nil || msg.Type != "ready" {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("worker not ready (got %+v, err %v)", msg, err)
	}

	w.healthy.Store(true)
	log.Printf("chrysalis: python worker ready (pid=%d)", cmd.Process.Pid)
	return w, nil
}

// Ping sends a ping and waits for pong (health check).
func (w *Worker) Ping() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.send(&Message{Type: "ping"}); err != nil {
		return err
	}
	msg, err := w.recv()
	if err != nil {
		return err
	}
	if msg.Type != "pong" {
		return fmt.Errorf("expected pong, got %q", msg.Type)
	}
	return nil
}

// Call sends a "call" request and returns the response message.
// It holds the mutex for the duration, so all calls are serialised.
func (w *Worker) Call(fn string, args []Arg, kwargs map[string]Arg) (*Message, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	id := fmt.Sprintf("req-%d", w.reqSeq.Add(1))
	req := &Message{
		ID:     id,
		Type:   "call",
		Func:   fn,
		Args:   args,
		Kwargs: kwargs,
	}
	if err := w.send(req); err != nil {
		return nil, err
	}
	resp, err := w.recv()
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Probe asks the worker whether a name refers to a module or callable.
func (w *Worker) Probe(target string) (*Message, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	id := fmt.Sprintf("probe-%d", w.reqSeq.Add(1))
	if err := w.send(&Message{ID: id, Type: "probe", Target: target}); err != nil {
		return nil, err
	}
	return w.recv()
}

// Shutdown asks the worker to exit gracefully and then kills the process.
func (w *Worker) Shutdown() {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.send(&Message{Type: "shutdown"})
	_, _ = w.recv()
	_ = w.conn.Close()
	_ = w.cmd.Process.Kill()
	_ = w.cmd.Wait()
}

// IsHealthy reports whether the worker is believed to be alive.
func (w *Worker) IsHealthy() bool {
	return w.healthy.Load()
}

// send writes a length-prefixed JSON message.
func (w *Worker) send(msg *Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint32(hdr, uint32(len(data)))
	if _, err := w.conn.Write(hdr); err != nil {
		w.healthy.Store(false)
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := w.conn.Write(data); err != nil {
		w.healthy.Store(false)
		return fmt.Errorf("write body: %w", err)
	}
	return nil
}

// recv reads a length-prefixed JSON message.
func (w *Worker) recv() (*Message, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(w.conn, hdr); err != nil {
		w.healthy.Store(false)
		return nil, fmt.Errorf("read header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr)
	if n > 16*1024*1024 {
		return nil, fmt.Errorf("message too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(w.conn, buf); err != nil {
		w.healthy.Store(false)
		return nil, fmt.Errorf("read body: %w", err)
	}
	var msg Message
	if err := json.Unmarshal(buf, &msg); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &msg, nil
}
