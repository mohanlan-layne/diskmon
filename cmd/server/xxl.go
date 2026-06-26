package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	xxl "github.com/xxl-job/xxl-job-executor-go"

	"diskmon/internal/config"
	"diskmon/internal/server/handler"
)

// startXXLExecutor starts an embedded XXL-JOB executor that auto-registers with
// the admin (addressType=0, like the other services) and exposes the
// "backfillCatalog" task. Run() is blocking, so it runs in a goroutine. Returns
// the executor for shutdown, or nil when disabled.
func startXXLExecutor(cfg config.XXLConfig, bf *handler.BackfillHandler, rc *handler.ReconcileHandler, logger *slog.Logger) xxl.Executor {
	if !cfg.Enabled {
		logger.Info("xxl-job executor disabled")
		return nil
	}

	opts := []xxl.Option{
		xxl.ServerAddr(cfg.AdminAddr),
		xxl.AccessToken(cfg.AccessToken),
		xxl.ExecutorPort(strconv.Itoa(cfg.ExecutorPort)),
		xxl.RegistryKey(cfg.AppName),
		xxl.SetLogger(&xxlLogger{l: logger}),
	}
	// In k8s, register the pod IP (downward API POD_IP) so the admin can call back
	// reliably instead of relying on the lib's interface auto-detection.
	if podIP := os.Getenv("POD_IP"); podIP != "" {
		opts = append(opts, xxl.ExecutorIp(podIP))
	}

	exec := xxl.NewExecutor(opts...)
	exec.Init()
	exec.RegTask("backfillCatalog", func(ctx context.Context, _ *xxl.RunReq) string {
		sum, err := bf.Run(ctx)
		if err != nil {
			logger.Error("backfillCatalog failed", "err", err)
			return "backfill error: " + err.Error()
		}
		logger.Info("backfillCatalog done", "summary", sum.String())
		return sum.String()
	})
	exec.RegTask("reconcileCatalog", func(ctx context.Context, req *xxl.RunReq) string {
		serverID := ""
		if req != nil {
			serverID = req.ExecutorParams
		}
		sum, err := rc.Run(ctx, serverID)
		if err != nil {
			logger.Error("reconcileCatalog failed", "err", err)
			return "reconcile error: " + err.Error()
		}
		logger.Info("reconcileCatalog done", "summary", sum.String())
		return sum.String()
	})

	go func() {
		if err := exec.Run(); err != nil {
			logger.Error("xxl-job executor stopped", "err", err)
		}
	}()
	logger.Info("xxl-job executor registered",
		"appname", cfg.AppName, "admin", cfg.AdminAddr, "port", cfg.ExecutorPort)
	return exec
}

// xxlLogger adapts slog to the executor SDK's Logger interface.
type xxlLogger struct{ l *slog.Logger }

func (x *xxlLogger) Info(format string, a ...any) {
	x.l.Info(fmt.Sprintf(format, a...))
}

func (x *xxlLogger) Error(format string, a ...any) {
	x.l.Error(fmt.Sprintf(format, a...))
}
