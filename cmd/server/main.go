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

//go:embed web/index.html
var indexHTML []byte

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

	// Web UI
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML) //nolint:errcheck
	})

	handler.NewServersHandler(db, cfg.AList).Register(r)
	handler.NewFilesHandler(db).Register(r)
	handler.NewClientsHandler(db).Register(r)
	dl := handler.NewDownloadHandler(db, cfg.SmbMounts)
	dl.Register(r)
	handler.NewTransformHandler(dl).Register(r)
	handler.NewBizHandler(db, dl).Register(r)
	kkURL := cfg.KkFileViewPublicURL // browser-reachable; falls back to internal
	if kkURL == "" {
		kkURL = cfg.KkFileViewURL
	}
	handler.NewPreviewHandler(db, kkURL).Register(r)
	handler.NewFileHandler(db, dl, kkURL).Register(r)

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
