// Package handler implements the HTTP API for Chrysalis.
package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bkmashiro/chrysalis/internal/pool"
	"github.com/bkmashiro/chrysalis/internal/safety"
)

// traceIDKey is the context key for the per-request trace ID.
type traceIDKey struct{}

// newTraceID returns a short hex token used to correlate logs and the
// response envelope for a single /run.
func newTraceID() string {
	b := make([]byte, 6) // 12 hex chars — collision-safe for daily request volumes
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RunRequest is the JSON body for POST /run.
type RunRequest struct {
	Code       string `json:"code"`
	Filter     string `json:"filter"`
	TimeoutSec int    `json:"timeout_sec"`
}

// RunResponse is the JSON response from POST /run.
type RunResponse struct {
	Status  string      `json:"status"`
	TraceID string      `json:"trace_id,omitempty"`
	Stdout  string      `json:"stdout,omitempty"`
	Stderr  string      `json:"stderr,omitempty"`
	Error   interface{} `json:"error"`
	Timing  TimingInfo  `json:"timing"`
}

// TimingInfo carries timing metrics.
type TimingInfo struct {
	TotalMs int64 `json:"total_ms"`
	ExecMs  int64 `json:"exec_ms"`
}

// Handler is the HTTP handler for Chrysalis.
type Handler struct {
	pool *pool.Pool
}

// New creates a new Handler backed by the given pool.
func New(p *pool.Pool) *Handler {
	return &Handler{pool: p}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/run" || r.Method != http.MethodPost {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.handleRun(w, r)
}

func (h *Handler) handleRun(w http.ResponseWriter, r *http.Request) {
	traceID := newTraceID()
	logger := slog.With("trace_id", traceID, "route", "/run")

	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.Warn("bad json", "err", err)
		writeError(w, traceID, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if strings.TrimSpace(req.Code) == "" {
		writeError(w, traceID, http.StatusBadRequest, "code is required")
		return
	}

	if req.TimeoutSec <= 0 {
		req.TimeoutSec = 30
	}

	// Safety pre-check.
	safetyResult := safety.Check(req.Code)
	if !safetyResult.Safe {
		logger.Info("blocked by safety pre-check", "blocked", safetyResult.Blocked)
		resp := RunResponse{
			Status:  "blocked",
			TraceID: traceID,
			Error:   fmt.Sprintf("safety check failed: %s", strings.Join(safetyResult.Blocked, "; ")),
			Timing:  TimingInfo{},
		}
		writeJSON(w, http.StatusForbidden, resp)
		return
	}

	filterProfile := req.Filter
	if filterProfile == "" {
		filterProfile = "autograder"
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.TimeoutSec+5)*time.Second)
	defer cancel()

	logger.Info("run starting",
		"filter", filterProfile,
		"timeout_sec", req.TimeoutSec,
		"code_len", len(req.Code),
	)
	t0 := time.Now()

	result, err := h.pool.Run(ctx, req.Code, filterProfile, req.TimeoutSec)
	if err != nil {
		logger.Error("run failed", "err", err, "elapsed_ms", time.Since(t0).Milliseconds())
		writeError(w, traceID, http.StatusInternalServerError, err.Error())
		return
	}

	var errField interface{} = nil
	status := "ok"
	if result.Error != "" {
		// User-friendlier error: drop the wazero "module closed with exit_code(N)"
		// noise and surface the trailing line of stderr (typically the Python
		// exception message) when present.
		msg := translateExecError(result.Error, result.Stderr)
		errField = msg
		status = "error"
	}

	logger.Info("run completed",
		"status", status,
		"latency_ms", result.Timing.TotalMs,
		"stdout_len", len(result.Stdout),
		"stderr_len", len(result.Stderr),
	)

	resp := RunResponse{
		Status:  status,
		TraceID: traceID,
		Stdout:  result.Stdout,
		Stderr:  result.Stderr,
		Error:   errField,
		Timing: TimingInfo{
			TotalMs: result.Timing.TotalMs,
			ExecMs:  result.Timing.ExecMs,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// translateExecError turns wazero-internal noise into something a user can act
// on. wazero reports a successful Python exit as "module closed with
// exit_code(0)" and a non-zero exit as "module closed with exit_code(N)";
// neither is useful to surface. When there is a Python traceback in stderr we
// prefer that.
func translateExecError(rawErr, stderr string) string {
	// Pull the last meaningful line of stderr (typically the exception type+msg).
	if line := lastLine(stderr); line != "" {
		return line
	}
	if strings.Contains(rawErr, "exit_code") {
		return "Python user code exited with a non-zero status"
	}
	if strings.Contains(rawErr, "context deadline exceeded") {
		return "execution timed out"
	}
	return rawErr
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n\r ")
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return strings.TrimSpace(s)
}

func writeError(w http.ResponseWriter, traceID string, code int, msg string) {
	writeJSON(w, code, RunResponse{
		Status:  "error",
		TraceID: traceID,
		Error:   msg,
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
