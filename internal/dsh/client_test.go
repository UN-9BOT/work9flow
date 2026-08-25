package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeDSH mimics a DSH HTTP API. We assert the Go adapter speaks the
// minimum subset work9flow needs without depending on DSH internals.
func fakeDSH(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, http.StatusCreated, map[string]string{"id": "sess-1"})
	})
	return httptest.NewServer(mux)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func TestClientHealth(t *testing.T) {
	dsh := fakeDSH(t)
	defer dsh.Close()
	c := NewClient(dsh.URL)
	st, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if st != "ok" {
		t.Errorf("status = %q", st)
	}
}

func TestClientHealthUnreachable(t *testing.T) {
	c := NewClient("http://127.0.0.1:1")
	_, err := c.Health(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable endpoint")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("want ErrUnreachable, got %v", err)
	}
}

func TestClientCreateSession(t *testing.T) {
	dsh := fakeDSH(t)
	defer dsh.Close()
	c := NewClient(dsh.URL)
	id, err := c.CreateSession(context.Background(), SessionRequest{Role: "planner"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if id != "sess-1" {
		t.Errorf("id = %q", id)
	}
}
