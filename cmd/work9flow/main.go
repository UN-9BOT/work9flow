// work9flow is the TUI client. It connects to a running work9flowd
// over HTTP, never embeds runtime state, and can be closed without
// affecting any active workflow run.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/log"

	"github.com/unbot/work9flow/internal/config"
	"github.com/unbot/work9flow/internal/protocol"
)

const appName = "work9flow"

func main() {
	var (
		configPath = flag.String("config", "", "path to work9flow.yaml (env: WORK9FLOW_CONFIG)")
		once       = flag.Bool("once", false, "render once and exit (no TUI loop)")
	)
	flag.Parse()

	logger := log.NewWithOptions(os.Stderr, log.Options{
		ReportTimestamp: true,
		Level:           log.WarnLevel,
	})

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Fatal("load config", "err", err)
	}

	base := cfg.RuntimeEndpoint

	if *once {
		// Non-interactive smoke path: useful for `make healthcheck`
		// and CI. No TUI, just print JSON to stdout.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h, err := fetchHealth(ctx, base)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: runtime unreachable at %s: %v\n", appName, base, err)
			os.Exit(2)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(h)
		return
	}

	model := newModel(base)
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		logger.Fatal("tui", "err", err)
	}
}

func fetchHealth(ctx context.Context, base string) (protocol.HealthResponse, error) {
	c := NewClient(base)
	return c.Health(ctx)
}
