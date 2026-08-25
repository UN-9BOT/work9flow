// work9flowd is the work9flow runtime/controller. It owns the local
// HTTP protocol surface, durable state, and (eventually) the DSH
// adapter. The TUI is a separate process that connects to it.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/charmbracelet/log"

	"github.com/unbot/work9flow/internal/agents"
	"github.com/unbot/work9flow/internal/config"
	"github.com/unbot/work9flow/internal/dsh"
	"github.com/unbot/work9flow/internal/engine"
	"github.com/unbot/work9flow/internal/engine/featuredev"
	"github.com/unbot/work9flow/internal/llm/localdsh"
	"github.com/unbot/work9flow/internal/providers"
	"github.com/unbot/work9flow/internal/protocol"
	"github.com/unbot/work9flow/internal/runtime"
	"github.com/unbot/work9flow/internal/runtime/worker"
	"github.com/unbot/work9flow/internal/storage"

	// Blank imports pin the planned Charm stack in go.mod. Live
	// callers land with MVP 03+; see README.md for the role of each.
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
	listen = stripScheme(listen)

	// Ensure the state directory exists before opening SQLite.
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		logger.Fatal("create state dir", "dir", cfg.StateDir, "err", err)
	}
	dbPath := filepath.Join(cfg.StateDir, "work9flow.db")
	repo, err := storage.OpenSQLite(dbPath)
	if err != nil {
		logger.Fatal("open storage", "path", dbPath, "err", err)
	}
	defer func() { _ = repo.Close() }()
	logger.Info("storage ready", "path", dbPath)

	var (
		eng      *engine.Engine
		ar       *agents.Runner
		inlineDS *httptest.Server // for shutdown when DSHEndpoint == ""
	)
	if cfg.DSHEndpoint != "" {
		c := dsh.NewClient(cfg.DSHEndpoint)
		hcCtx, hcCancel := context.WithTimeout(context.Background(), 3*time.Second)
		st, err := c.Health(hcCtx)
		hcCancel()
		if err != nil {
			logger.Warn("DSH unreachable; engine+worker disabled",
				"endpoint", cfg.DSHEndpoint, "err", err)
		} else {
			logger.Info("DSH reachable", "endpoint", cfg.DSHEndpoint, "status", st)
			ar = agents.New(c, repo)
		}
	} else if cfg.ProvidersFile != "" {
		pf, err := providers.LoadFile(cfg.ProvidersFile)
		if err != nil {
			logger.Fatal("load providers file", "path", cfg.ProvidersFile, "err", err)
		}
		if len(pf.Providers) == 0 {
			logger.Fatal("providers file has no providers", "path", cfg.ProvidersFile)
		}
		defaultRef, refErr := pickDefaultProvider(pf, cfg.ModelRoles)
		if refErr != nil {
			logger.Fatal("pick default provider", "err", refErr)
		}
		pd, _, lookupErr := pf.Lookup(defaultRef)
		if lookupErr != nil {
			logger.Fatal("lookup provider", "err", lookupErr)
		}
		srv, srvErr := newInlineDSH(pd, defaultRef.Model)
		if srvErr != nil {
			logger.Fatal("start inline DSH", "err", srvErr)
		}
		inlineDS = srv
		c := dsh.NewClient(srv.URL)
		logger.Info("inline DSH started",
			"endpoint", srv.URL,
			"provider", defaultRef.Provider,
			"model", defaultRef.Model)
		ar = agents.New(c, repo)
	} else {
		logger.Info("DSH endpoint not configured; running as pure CRUD service")
	}

	if ar != nil {
		ar.PollInterval = 5 * time.Second
		ar.PollBudget = 60 * time.Second
		eng = engine.New(engine.Option{Repo: repo})
		if err := eng.RegisterWorkflow(featuredev.Workflow(ar)); err != nil {
			logger.Fatal("register feature-development workflow", "err", err)
		}
		logger.Info("engine wired",
			"workflow", featuredev.Workflow(ar).ID,
			"version", featuredev.Workflow(ar).Version)
	}

	srv := runtime.New(runtime.Options{
		Name:    "work9flowd",
		Version: protocol.Version,
		Addr:    listen,
		Logger:  logger,
		Repo:    repo,
	})

	ln, err := srv.Listen()
	if err != nil {
		logger.Fatal("listen", "addr", listen, "err", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	if eng != nil && ar != nil {
		go worker.New(worker.Options{
			Engine:  eng,
			Repo:    repo,
			Logger:  logger,
			Interval: 2 * time.Second,
		}).Run(ctx)
	}

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
	if inlineDS != nil {
		inlineDS.Close()
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

// pickDefaultProvider picks the "default" model reference from
// providers.ModelRoles; if no default is configured, the first
// provider's default_model is used.
func pickDefaultProvider(pf providers.File, modelRoles map[string]string) (providers.ProviderRef, error) {
	if v, ok := modelRoles["default"]; ok && v != "" {
		return providers.ParseRef(v)
	}
	if v, ok := modelRoles["implementer"]; ok && v != "" {
		// Fall back to the implementer role's model so workflow stages
		// drive against the same model unless explicitly told otherwise.
		return providers.ParseRef(v)
	}
	for name, p := range pf.Providers {
		if p.DefaultModel != "" {
			return providers.ParseRef(p.DefaultModel)
		}
		_ = name
	}
	return providers.ProviderRef{}, fmt.Errorf("no default model: set model_roles.default or providers.{x}.default_model")
}

// newInlineDSH boots a localdsh server bound to one provider/model.
// The server listens on a random localhost port; the caller must Close
// it when shutting down.
func newInlineDSH(p providers.Provider, model string) (*httptest.Server, error) {
	s, err := localdsh.New(localdsh.Provider{
		BaseURL:   p.BaseURL,
		APIKeyEnv: p.APIKeyEnv,
		Model:     model,
	})
	if err != nil {
		return nil, err
	}
	return httptest.NewServer(s.Handler()), nil
}
