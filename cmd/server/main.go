// diskmon-server provides the HTTP API for file catalog queries, server configuration
// management, and file operations (download, ZIP, watermark, merge, preview).
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "github.com/go-sql-driver/mysql"

	"diskmon/internal/config"
	"diskmon/internal/server/handler"
)

//go:embed web/admin.html
var adminHTML []byte

//go:embed web/login.html
var loginHTML []byte

func main() {
	cfgPath := flag.String("config", "server-config.yaml", "path to server config YAML")
	flag.Parse()

	cfg, err := config.LoadServer(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	logger := slog.Default()

	db, err := sql.Open("mysql", cfg.Postgres.DSN)
	if err != nil {
		logger.Error("open db failed", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		logger.Error("connect db failed", "err", err)
		os.Exit(1)
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	serveHTML := func(body []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(body) //nolint:errcheck
		}
	}

	admin := handler.NewAdminHandler(cfg.Admin.User, cfg.Admin.Password)

	// Root has no public UI: send visitors to the management login.
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	// Public auth endpoints: login form (GET) and login/logout (POST).
	r.Get("/admin/login", serveHTML(loginHTML))
	admin.Register(r)

	kkURL := cfg.KkFileViewPublicURL // browser-reachable; falls back to internal
	if kkURL == "" {
		kkURL = cfg.KkFileViewURL
	}

	// Health check (used by K8s probes — must be fast).
	handler.NewHealthHandler(db).Register(r)

	// Public data APIs (anonymous) — file catalog queries, download/zip/preview.
	handler.NewFilesHandler(db).Register(r)
	dl := handler.NewDownloadHandler(db, cfg.SmbMounts)
	dl.Register(r)
	handler.NewTransformHandler(dl).Register(r)
	handler.NewBizHandler(db, dl).Register(r)
	handler.NewPreviewHandler(db, kkURL).Register(r)
	handler.NewFileHandler(db, dl, kkURL).Register(r)
	handler.NewDrawingHandler(db, dl, kkURL).Register(r)

	// Catalog backfill: repairs NULL-size / wrong-is_dir rows via AList.
	backfill := handler.NewBackfillHandler(db, cfg.XXL.JobToken)
	backfill.Register(r)
	// Catalog reconcile: walks AList mount tree and upserts all entries (no truncation).
	reconcile := handler.NewReconcileHandler(db, cfg.XXL.JobToken)
	reconcile.Register(r)
	if exec := startXXLExecutor(cfg.XXL, backfill, reconcile, logger); exec != nil {
		defer exec.Stop()
	}

	// Management group (login required) — UI page plus server/client config APIs.
	r.Group(func(r chi.Router) {
		r.Use(admin.RequireAuth)
		r.Get("/admin/", serveHTML(adminHTML))
		r.Get("/admin", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin/", http.StatusFound)
		})
		handler.NewServersHandler(db, cfg.AList).Register(r)
		handler.NewClientsHandler(db).Register(r)
		handler.NewRefreshDirHandler(db, cfg.SmbMounts).Register(r)
	})

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: r,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	logger.Info("diskmon-server starting", "addr", cfg.Listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
	logger.Info("diskmon-server stopped")
}
