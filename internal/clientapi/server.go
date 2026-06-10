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
	"time"

	"diskmon/internal/config"
)

// RescanFunc is called by the rescan endpoint.
type RescanFunc func(ctx context.Context) error

// Server is the local HTTP management server embedded in the client.
type Server struct {
	cfg     *config.ClientConfig
	cfgPath string
	rescan  RescanFunc
}

// New creates a Server. cfgPath is the path of the config file on disk, used by
// the /config endpoint to persist remote edits.
func New(cfg *config.ClientConfig, cfgPath string, rescan RescanFunc) *Server {
	return &Server{cfg: cfg, cfgPath: cfgPath, rescan: rescan}
}

// Start registers routes and begins serving on cfg.API.Listen.
// It runs until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/logs", s.authMiddleware(s.handleLogs))
	mux.HandleFunc("/logs/clear", s.authMiddleware(s.handleClearLogs))
	mux.HandleFunc("/rescan", s.authMiddleware(s.handleRescan))
	mux.HandleFunc("/config", s.authMiddleware(s.handleConfig))

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
	writeJSON(w, map[string]string{"status": "ok"})
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

// handleClearLogs truncates the client log file to zero length.
// POST /logs/clear
func (s *Server) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// The logger appends, so truncating to 0 makes the next line start at the
	// beginning — no holes.
	if err := os.Truncate(s.cfg.LogPath, 0); err != nil {
		http.Error(w, "cannot clear log: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "log cleared"})
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
	writeJSON(w, map[string]string{"status": "rescan started"})
}

// volumeView is the read-only volume info returned by GET /config.
type volumeView struct {
	Name    string `json:"name"`
	BizRule string `json:"biz_rule"`
}

// configView is the editable + read-only config returned by GET /config.
type configView struct {
	ServerID       string               `json:"server_id"`
	Name           string               `json:"name"`
	CheckpointPath string               `json:"checkpoint_path"`
	LogPath        string               `json:"log_path"`
	PollIntervalMs int                  `json:"poll_interval_ms"`
	RenameWindowMs int                  `json:"rename_window_ms"`
	Volumes        []volumeView         `json:"volumes"`
	Filters        config.FiltersConfig `json:"filters"`
}

// configPatch carries the editable fields for PUT /config. Pointers distinguish
// "field omitted" from "set to empty".
type configPatch struct {
	Name           *string   `json:"name"`
	IncludeDirs    *[]string `json:"include_dirs"`
	ExcludeDirs    *[]string `json:"exclude_dirs"`
	Extensions     *[]string `json:"extensions"`
	Events         *[]string `json:"events"`
	PollIntervalMs *int      `json:"poll_interval_ms"`
	RenameWindowMs *int      `json:"rename_window_ms"`
}

// handleConfig serves the editable client parameters.
//
//	GET  /config  → current parameters
//	PUT  /config  → apply edits, persist, and exit so the service restarts with
//	                the new config (monitoring parameters only take effect on
//	                restart). Volumes and biz_rule are read-only here.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		vols := make([]volumeView, 0, len(s.cfg.Volumes))
		for _, v := range s.cfg.Volumes {
			vols = append(vols, volumeView{Name: v.Name, BizRule: v.BizRule})
		}
		writeJSON(w, configView{
			ServerID:       s.cfg.ServerID,
			Name:           s.cfg.Name,
			CheckpointPath: s.cfg.CheckpointPath,
			LogPath:        s.cfg.LogPath,
			PollIntervalMs: s.cfg.PollIntervalMs,
			RenameWindowMs: s.cfg.RenameWindowMs,
			Volumes:        vols,
			Filters:        s.cfg.Filters,
		})
	case http.MethodPut:
		var p configPatch
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		applyPatch(s.cfg, p)
		// Validate before persisting so a bad edit can't leave the client unable
		// to start after restart.
		if err := config.ValidateClient(s.cfg); err != nil {
			http.Error(w, "invalid config: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := config.SaveClient(s.cfgPath, s.cfg); err != nil {
			http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "config saved; client restarting to apply"})
		// Exit after the response flushes; the Windows service auto-restarts and
		// loads the new config.
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func applyPatch(cfg *config.ClientConfig, p configPatch) {
	if p.Name != nil {
		cfg.Name = *p.Name
	}
	if p.IncludeDirs != nil {
		cfg.Filters.IncludeDirs = *p.IncludeDirs
	}
	if p.ExcludeDirs != nil {
		cfg.Filters.ExcludeDirs = *p.ExcludeDirs
	}
	if p.Extensions != nil {
		cfg.Filters.Extensions = *p.Extensions
	}
	if p.Events != nil {
		cfg.Filters.Events = *p.Events
	}
	if p.PollIntervalMs != nil {
		cfg.PollIntervalMs = *p.PollIntervalMs
	}
	if p.RenameWindowMs != nil {
		cfg.RenameWindowMs = *p.RenameWindowMs
	}
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
