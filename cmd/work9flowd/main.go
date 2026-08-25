// work9flowd is the work9flow runtime/controller. It owns the local
// HTTP protocol surface, durable state and (eventually) the DSH
// adapter. The TUI is a separate process that connects to it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/charmbracelet/log"

	"github.com/unbot/work9flow/internal/config"
	"github.com/unbot/work9flow/internal/dsh"
	"github.com/unbot/work9flow/internal/protocol"
	"github.com/unbot/work9flow/internal/runtime"

	// Blank imports pin the planned Charm stack in go.mod. Live
	// callers land with MVP 02+; see README.md for the role of each.
	_ "github.com/catppuccin/go"
	_ "github.com/charmbracelet/bubbletea"
	_ "github.com/charmbracelet/glamour"
	_ "github.com/charmbracelet/huh"
	_ "github.com/charmbracelet/lipgloss"
)

func main() {
	var (
		configPath = flag.String("config", "", "path to work9flow.yaml (env: WORK9FLOW_CONFIG)")
		addr       = flag.String("addr", "", "override runtime listen address (default from config)")
	)
	flag.Parse()

	logger := log.NewWithOptions(os.Stderr, log.Options{
		ReportTimestamp: true,
		Level:           log.InfoLevel,
	})
	log.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatal("load config", "err", err)
	}

	listen := *addr
	if listen == "" {
		listen = cfg.RuntimeEndpoint
	}
	// Config value may be "http://host:port" or "host:port"; strip scheme.
		listen = stripScheme(listen)

	if cfg.DSHEndpoint != "" {
		c := dsh.NewClient(cfg.DSHEndpoint)
		if st, err := c.Health(context.Background()); err != nil {
			logger.Warn("DSH unreachable; adapter disabled", "endpoint", cfg.DSHEndpoint, "err", err)
		} else {
			logger.Info("DSH reachable", "endpoint", cfg.DSHEndpoint, "status", st)
		}
	}

	srv := runtime.New(runtime.Options{
		Name:    "work9flowd",
		Version: protocol.Version,
		Addr:    listen,
		Logger:  logger,
	})

	ln, err := srv.Listen()
	if err != nil {
		logger.Fatal("listen", "addr", listen, "err", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			logger.Error("server exited", "err", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "err", err)
	}
	fmt.Fprintln(os.Stderr, "work9flowd: stopped")
}

func stripScheme(addr string) string {
	for _, p := range []string{"http://", "https://"} {
		if len(addr) > len(p) && addr[:len(p)] == p {
			return addr[len(p):]
		}
	}
	return addr
}
