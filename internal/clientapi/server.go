//go:build windows

// Package clientapi implements the local HTTP management API embedded in the client.
package clientapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"diskmon/internal/config"
)

// RescanFunc is called by the restart/rescan endpoints.
type RescanFunc func(ctx context.Context) error

// Server is the local HTTP management server embedded in the client.
type Server struct {
	cfg    *config.ClientConfig
	rescan RescanFunc
}

// New creates a Server.
func New(cfg *config.ClientConfig, rescan RescanFunc) *Server {
	return &Server{cfg: cfg, rescan: rescan}
}

// Start registers routes and begins serving on cfg.API.Listen.
// It runs until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/logs", s.authMiddleware(s.handleLogs))
	mux.HandleFunc("/rescan", s.authMiddleware(s.handleRescan))
	mux.HandleFunc("/restart", s.authMiddleware(s.handleRestart))
	mux.HandleFunc("/health", s.handleHealth)

	srv := &http.Server{
		Addr:    s.cfg.API.Listen,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("clientapi: %w", err)
	}
	return nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
}

// handleLogs tails the client log file.
// GET /logs?lines=100
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 100
	if v := r.URL.Query().Get("lines"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}

	data, err := os.ReadFile(s.cfg.LogPath)
	if err != nil {
		http.Error(w, "cannot read log: "+err.Error(), http.StatusInternalServerError)
		return
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, strings.Join(lines, "\n"))
}

// handleRescan triggers a full rescan in a background goroutine.
// POST /rescan
func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.rescan == nil {
		http.Error(w, "rescan not configured", http.StatusServiceUnavailable)
		return
	}
	go func() {
		_ = s.rescan(context.Background())
	}()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "rescan started"}) //nolint:errcheck
}

// handleRestart exits the process; the Windows Service manager will restart it.
// POST /restart
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "restarting"}) //nolint:errcheck

	// Flush the response before exiting.
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	os.Exit(0)
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.API.Token == "" {
			next(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != s.cfg.API.Token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

