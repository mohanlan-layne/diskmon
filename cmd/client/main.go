//go:build windows

// diskmon-client monitors NTFS volumes via USN Journal and syncs file metadata
// to MySQL. Runs as a Windows Service.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/debug"
	"golang.org/x/sys/windows/svc/eventlog"

	"diskmon/internal/catalog"
	"diskmon/internal/clientapi"
	"diskmon/internal/config"
	"diskmon/internal/filter"
	"diskmon/internal/scanner"
	"diskmon/internal/watcher/usn"
)

const serviceName = "diskmon-client"

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to client config YAML")
	rescan := flag.Bool("rescan", false, "force a full rescan before monitoring")
	runSvc := flag.Bool("service", false, "run as Windows Service")
	printInfo := flag.Bool("print-info", false, "print server registration info and exit")
	flag.Parse()

	cfg, err := config.LoadClient(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	if *printInfo {
		printRegistrationInfo(cfg)
		return
	}

	logger := buildLogger(cfg.LogPath)
	printRegistrationInfo(cfg) // always print on startup so it shows in logs

	if *runSvc {
		runAsService(cfg, *rescan, logger)
		return
	}
	if err := run(context.Background(), cfg, *rescan, logger); err != nil {
		logger.Error("client exited with error", "err", err)
		os.Exit(1)
	}
}

// printRegistrationInfo prints a copy-paste block for registering this client
// in the diskmon server UI (POST /api/servers).
func printRegistrationInfo(cfg *config.ClientConfig) {
	localIP := detectLocalIP()
	apiPort := cfg.API.Listen
	if !strings.Contains(apiPort, ".") {
		// ":19090" → "192.168.x.x:19090"
		apiPort = localIP + apiPort
	}

	volNames := make([]string, 0, len(cfg.Volumes))
	for _, v := range cfg.Volumes {
		volNames = append(volNames, v.Name)
	}

	name := cfg.Name
	if name == "" {
		name = cfg.ServerID
	}

	reg := map[string]any{
		"server_id": cfg.ServerID,
		"name":      name,
		"smb_host":  localIP,
		"smb_user":  cfg.SmbUser,
		"api_addr":  apiPort,
		"api_token": cfg.API.Token,
	}
	regJSON, _ := json.MarshalIndent(reg, "  ", "  ")

	line := strings.Repeat("═", 60)
	fmt.Println()
	fmt.Println(line)
	fmt.Println("  diskmon-client 注册信息")
	fmt.Println("  复制下方内容到 diskmon 管理界面「添加服务器」")
	fmt.Println(line)
	fmt.Printf("  Server ID  : %s\n", cfg.ServerID)
	fmt.Printf("  名称       : %s\n", name)
	fmt.Printf("  本机 IP    : %s\n", localIP)
	fmt.Printf("  管理 API   : %s\n", apiPort)
	if cfg.API.Token != "" {
		fmt.Printf("  API Token  : %s\n", cfg.API.Token)
	} else {
		fmt.Printf("  API Token  : (空，无鉴权)\n")
	}
	fmt.Printf("  SMB 用户   : %s\n", cfg.SmbUser)
	fmt.Printf("  监控卷     : %s\n", strings.Join(volNames, ", "))
	fmt.Println()
	fmt.Println("  ── 注册 JSON (POST /api/servers) ──────────────────────")
	fmt.Printf("  %s\n", string(regJSON))
	fmt.Println(line)
	fmt.Println()
}

// detectLocalIP returns the preferred outbound IP of this machine.
func detectLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func run(ctx context.Context, cfg *config.ClientConfig, rescan bool, logger *slog.Logger) error {
	db, err := sql.Open("mysql", cfg.Postgres.DSN)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(3)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("connect db: %w", err)
	}

	cat := catalog.New(db)

	if err := cat.EnsurePartition(ctx, cfg.ServerID); err != nil {
		return fmt.Errorf("ensure partition: %w", err)
	}

	rules := buildRules(cfg)

	f := &filter.Filter{
		IncludeDirs: cfg.Filters.IncludeDirs,
		ExcludeDirs: cfg.Filters.ExcludeDirs,
		Extensions:  cfg.Filters.Extensions,
		Events:      cfg.Filters.Events,
	}

	s := scanner.New(cfg.ServerID, cfg.Volumes, rules, cat, f, logger)

	if rescan {
		logger.Info("starting full scan")
		if err := s.Run(ctx); err != nil {
			return fmt.Errorf("full scan: %w", err)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup

	if cfg.API.Listen != "" {
		api := clientapi.New(cfg, func(ctx context.Context) error {
			return s.Run(ctx)
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := api.Start(ctx); err != nil {
				logger.Error("management API exited", "err", err)
			}
		}()
	}

	for _, vol := range cfg.Volumes {
		vol := vol
		rule := rules.Get(vol.BizRule)
		m := usn.NewMonitor(usn.MonitorConfig{
			ServerID:       cfg.ServerID,
			Volume:         vol,
			Filter:         f,
			Rule:           rule,
			Catalog:        cat,
			CheckpointDir:  cfg.CheckpointPath,
			PollIntervalMs: cfg.PollIntervalMs,
			RenameWindowMs: cfg.RenameWindowMs,
			CacheSize:      cfg.PathResolverCacheSize,
			Logger:         logger,
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Run(ctx); err != nil && ctx.Err() == nil {
				logger.Error("monitor exited", "volume", vol.Name, "err", err)
				cancel()
			}
		}()
	}

	wg.Wait()
	return nil
}

type windowsService struct {
	cfg    *config.ClientConfig
	rescan bool
	logger *slog.Logger
}

func (s *windowsService) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const cmdsAccepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- run(ctx, s.cfg, s.rescan, s.logger) }()

	status <- svc.Status{State: svc.Running, Accepts: cmdsAccepted}

	for {
		select {
		case err := <-errCh:
			if err != nil {
				s.logger.Error("service error", "err", err)
				return false, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
			}
		}
	}
}

func buildLogger(logPath string) *slog.Logger {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		slog.Default().Warn("cannot create log dir, logging to stdout only", "err", err)
		return slog.Default()
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Default().Warn("cannot open log file, logging to stdout only", "err", err, "path", logPath)
		return slog.Default()
	}
	w := io.MultiWriter(os.Stdout, f)
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func runAsService(cfg *config.ClientConfig, rescan bool, logger *slog.Logger) {
	elog, err := eventlog.Open(serviceName)
	if err != nil {
		elog, _ = eventlog.Open("")
	}
	if elog != nil {
		defer elog.Close()
	}

	isInteractive, err := svc.IsAnInteractiveSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot determine session type: %v\n", err)
		os.Exit(1)
	}

	handler := &windowsService{cfg: cfg, rescan: rescan, logger: logger}

	var runFn func(string, svc.Handler) error
	if isInteractive {
		runFn = debug.Run
	} else {
		runFn = svc.Run
	}

	if err := runFn(serviceName, handler); err != nil {
		fmt.Fprintf(os.Stderr, "service failed: %v\n", err)
		os.Exit(1)
	}
}
