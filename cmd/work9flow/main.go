// work9flow is the TUI client. It connects to a running work9flowd
// over HTTP, never embeds runtime state, and can be closed without
// affecting any active workflow run.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
	catppuccin "github.com/catppuccin/go"

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

	model := newModel(base, logger)
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		logger.Fatal("tui", "err", err)
	}
}

func fetchHealth(ctx context.Context, base string) (protocol.HealthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/v1/health", nil)
	if err != nil {
		return protocol.HealthResponse{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return protocol.HealthResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return protocol.HealthResponse{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var out protocol.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return protocol.HealthResponse{}, err
	}
	return out, nil
}

// ---- bubbletea model ----

type tickMsg time.Time
type healthMsg struct {
	resp protocol.HealthResponse
	err  error
}

type model struct {
	base   string
	logger *log.Logger
	health protocol.HealthResponse
	err    error
	loaded bool
	quitting bool
}

func newModel(base string, logger *log.Logger) model {
	return model{base: base, logger: logger}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.pollHealth(), tick())
}

func (m model) pollHealth() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		h, err := fetchHealth(ctx, m.base)
		return healthMsg{resp: h, err: err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			m.quitting = true
			return m, tea.Quit
		}
	case healthMsg:
		m.loaded = true
		m.health = msg.resp
		m.err = msg.err
	case tickMsg:
		return m, m.pollHealth()
	}
	return m, nil
}

func (m model) View() string {
	palette := catppuccin.Mocha
	header := lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color(palette.Peach().Hex)).
		Padding(0, 1).
		Render(appName + " — TUI client")

	status := lipgloss.NewStyle().
		Foreground(lipgloss.Color(palette.Subtext1().Hex)).
		Render(fmt.Sprintf("runtime: %s", m.base))

	var body string
	switch {
	case !m.loaded:
		body = lipgloss.NewStyle().
			Foreground(lipgloss.Color(palette.Yellow().Hex)).
			Render("connecting to runtime…")
	case m.err != nil:
		body = lipgloss.NewStyle().
			Foreground(lipgloss.Color(palette.Red().Hex)).
			Render(fmt.Sprintf("runtime unreachable: %v", m.err))
	default:
		ok := lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Green().Hex)).Render("●")
		ok += fmt.Sprintf("  status=%s  version=%s  uptime=%ds",
			m.health.Status, m.health.Version, m.health.UptimeS)
		body = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Text().Hex)).Render(ok)
	}

	help := lipgloss.NewStyle().
		Foreground(lipgloss.Color(palette.Overlay1().Hex)).
		Render("press q to disconnect (does not stop the runtime)")

	return lipgloss.JoinVertical(lipgloss.Left, header, status, "", body, "", help)
}
