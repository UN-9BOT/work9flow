// Package runtime hosts work9flowd's HTTP server, lifecycle and
// MVP-level handlers. It owns no business logic yet — only the
// transport and a stable health/version contract.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/charmbracelet/log"

	"github.com/unbot/work9flow/internal/protocol"
)

// Options configures a Server.
type Options struct {
	Name    string
	Version string
	Addr    string // "host:port" or ":port"; ":0" picks a free port.
	Logger  *log.Logger
}

// Server is the work9flowd HTTP frontend. It is safe to construct
// once and reuse across restarts via Shutdown + new Listen/Serve.
type Server struct {
	opts   Options
	mux    *http.ServeMux
	http   *http.Server
	mu     sync.Mutex
	addr   string
	bootAt time.Time
	logger *log.Logger
}

// New returns a Server configured from opts.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	mux := http.NewServeMux()
	s := &Server{
		opts:   opts,
		mux:    mux,
		bootAt: time.Now(),
		logger: logger,
	}
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.HandleFunc("GET /v1/runs", s.handleRuns)
	mux.Handle("/", http.HandlerFunc(jsonNotFound))
	return s
}

// Listen binds the configured address. Returns the listener so the
// caller can pass it to Serve. With Addr ":0" the chosen port is
// observable via Addr() after Listen returns.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", s.opts.Addr, err)
	}
	s.mu.Lock()
	s.addr = ln.Addr().String()
	s.mu.Unlock()
	return ln, nil
}

// Serve runs the HTTP loop on ln until Shutdown is called.
func (s *Server) Serve(ln net.Listener) error {
	s.http = &http.Server{
		Handler:      s.mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	s.logger.Info("runtime listening", "addr", ln.Addr().String())
	err := s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Addr returns the bound address or "" if not listening yet.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Shutdown stops the server and waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.HealthResponse{
		Status:  "ok",
		Version: s.opts.Version,
		UptimeS: int64(time.Since(s.bootAt).Seconds()),
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.VersionResponse{
		Name:    s.opts.Name,
		Version: s.opts.Version,
	})
}

func (s *Server) handleRuns(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.RunListResponse{Runs: nil})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
