// Package handler implements the HTTP API for Chrysalis.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/bkmashiro/chrysalis/internal/pool"
	"github.com/bkmashiro/chrysalis/internal/safety"
)

// RunRequest is the JSON body for POST /run.
type RunRequest struct {
	Code       string `json:"code"`
	Filter     string `json:"filter"`
	TimeoutSec int    `json:"timeout_sec"`
}

// RunResponse is the JSON response from POST /run.
type RunResponse struct {
	Status string      `json:"status"`
	Stdout string      `json:"stdout,omitempty"`
	Stderr string      `json:"stderr,omitempty"`
	Error  interface{} `json:"error"`
	Timing TimingInfo  `json:"timing"`
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
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if strings.TrimSpace(req.Code) == "" {
		writeError(w, http.StatusBadRequest, "code is required")
		return
	}

	if req.TimeoutSec <= 0 {
		req.TimeoutSec = 30
	}

	// Safety pre-check.
	safetyResult := safety.Check(req.Code)
	if !safetyResult.Safe {
		resp := RunResponse{
			Status: "blocked",
			Error:  fmt.Sprintf("safety check failed: %s", strings.Join(safetyResult.Blocked, "; ")),
			Timing: TimingInfo{},
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

	log.Printf("chrysalis: run filter=%s timeout=%ds code_len=%d", filterProfile, req.TimeoutSec, len(req.Code))

	result, err := h.pool.Run(ctx, req.Code, filterProfile, req.TimeoutSec)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var errField interface{} = nil
	status := "ok"
	if result.Error != "" {
		errField = result.Error
		status = "error"
	}

	resp := RunResponse{
		Status: status,
		Stdout: result.Stdout,
		Stderr: result.Stderr,
		Error:  errField,
		Timing: TimingInfo{
			TotalMs: result.Timing.TotalMs,
			ExecMs:  result.Timing.ExecMs,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, RunResponse{
		Status: "error",
		Error:  msg,
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
