package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/unbot/work9flow/internal/protocol"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv := New(Options{Name: "work9flowd-test", Version: "test", Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	ln, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()

	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Addr() == "" {
		t.Fatal("server did not bind in time")
	}
	return srv, "http://" + srv.Addr()
}

func TestHealthEndpoint(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Get(base + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got protocol.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("Status = %q", got.Status)
	}
	if got.Version == "" {
		t.Error("Version must not be empty")
	}
	if got.UptimeS < 0 {
		t.Errorf("UptimeS = %d", got.UptimeS)
	}
}

func TestVersionEndpoint(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Get(base + "/v1/version")
	if err != nil {
		t.Fatalf("GET /v1/version: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got protocol.VersionResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "work9flowd-test" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Version != "test" {
		t.Errorf("Version = %q", got.Version)
	}
}

func TestUnknownRoute404(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Get(base + "/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("expected json content-type, got %q", resp.Header.Get("Content-Type"))
	}
}

func TestRunsEndpointEmpty(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Get(base + "/v1/runs")
	if err != nil {
		t.Fatalf("GET /v1/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got protocol.RunListResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Runs) != 0 {
		t.Errorf("expected 0 runs at bootstrap, got %d", len(got.Runs))
	}
}

func TestShutdownStopsServing(t *testing.T) {
	srv := New(Options{Name: "x", Version: "v", Addr: "127.0.0.1:0"})
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
