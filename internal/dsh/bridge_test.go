package dsh_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/dsh"
)

// fakeBridge is an httptest server that mimics the runtime/dsh-bridge
// HTTP API. It records each request and serves scripted responses so
// tests can assert both sides of the wire without standing up Node.
//
// The fake emits upstream-shape BridgeEvent frames (Type=session.event
// with `data` carrying the upstream SessionEvent envelope, NOT the
// legacy envelope-wrapped session.event with `event` field).
type fakeBridge struct {
	mu                 sync.Mutex
	sessions           map[string]bool
	prompts            map[string]int
	runScripts         map[string][]dsh.RawBridgeEvent // run streams keyed by sessionID
	script             []dsh.RawBridgeEvent            // events for /events (legacy)
	emitTransportError string                         // non-empty → emit bridge.transport_error frame instead of script
	closed             bool
	shutdownHit        bool
	srv                *httptest.Server
}

func newFakeBridge(t *testing.T) *fakeBridge {
	t.Helper()
	fb := &fakeBridge{
		sessions:   map[string]bool{},
		prompts:    map[string]int{},
		runScripts: map[string][]dsh.RawBridgeEvent{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "serverInfo": map[string]string{
			"name":    "dsh-bridge-test",
			"version": "test",
		}})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		var req dsh.CreateSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode CreateSessionRequest: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		id := fmt.Sprintf("sess-%s", req.Model)
		fb.sessions[id] = true
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": id,
			"serverInfo": map[string]string{"name": "dsh-bridge-test", "version": "test"},
		})
	})
	mux.HandleFunc("POST /sessions/{id}/prompt", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		fb.mu.Lock()
		fb.prompts[id]++
		fb.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"messageId": "msg-" + id})
	})
	// /run — owned Activity interval. Emits run.start with the
	// messageId, then forwards each scripted upstream notification,
	// then closes with run.end{reason=idle}.
	mux.HandleFunc("POST /sessions/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fb.mu.Lock()
		script := append([]dsh.RawBridgeEvent(nil), fb.runScripts[id]...)
		fb.mu.Unlock()
		write := func(ev dsh.RawBridgeEvent) {
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		write(dsh.RawBridgeEvent{Type: "run.start", MessageID: "msg-" + id})
		for _, ev := range script {
			write(ev)
		}
		write(dsh.RawBridgeEvent{Type: "run.end", Reason: "idle"})
	})
	mux.HandleFunc("GET /sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fb.mu.Lock()
		transportErr := fb.emitTransportError
		evs := append([]dsh.RawBridgeEvent(nil), fb.script...)
		fb.mu.Unlock()
		if transportErr != "" {
			// Emit the explicit bridge transport-control frame, then
			// close the stream. The Go reader must route this to errCh
			// without Normalize() (which would mask it as raw.passthrough).
			frame, _ := json.Marshal(map[string]string{"type": "bridge.transport_error", "message": transportErr})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
			flusher.Flush()
			return
		}
		for _, ev := range evs {
			if ev.SessionID != "" && ev.SessionID != id {
				continue
			}
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	})
	mux.HandleFunc("POST /sessions/{id}/close", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":  "not_supported",
			"detail": "upstream DSH SDK has no per-session close; shutdown the runtime instead",
		})
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, _ *http.Request) {
		fb.mu.Lock()
		fb.shutdownHit = true
		fb.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	fb.srv = httptest.NewServer(mux)
	t.Cleanup(fb.srv.Close)
	return fb
}

func TestNewBridgeStripsTrailingSlash(t *testing.T) {
	b := dsh.NewBridge("http://127.0.0.1:1/")
	if b == nil {
		t.Fatal("nil bridge")
	}
}

func TestBridgeHealth(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	st, err := b.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if st.Status != "ready" {
		t.Errorf("status = %q, want ready", st.Status)
	}
	if st.ServerInfo.Name != "dsh-bridge-test" {
		t.Errorf("serverInfo.name = %q", st.ServerInfo.Name)
	}
}

func TestBridgeHealthUnreachable(t *testing.T) {
	b := dsh.NewBridge("http://127.0.0.1:1")
	_, err := b.Health(context.Background())
	if !errors.Is(err, dsh.ErrUnreachable) {
		t.Errorf("err = %v, want ErrUnreachable", err)
	}
}

func TestBridgeCreateSession(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	ref, err := b.CreateSession(context.Background(), dsh.CreateSessionRequest{Cwd: "/x", Provider: "deepseek", Model: "deepseek-chat"})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID != "sess-deepseek-chat" {
		t.Errorf("ID = %q", ref.ID)
	}
}

// TestRunEOFWithoutRunEndYieldsErrRunIncomplete covers reviewer P1 #2:
// if the bridge closes the SSE stream before emitting run.end, the
// caller must see ErrRunIncomplete — never a fabricated success from
// a stray assistant/message.
func TestRunEOFWithoutRunEndYieldsErrRunIncomplete(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sessionId":"s1","serverInfo":{"name":"x","version":"0"}}`))
	})
	mux.HandleFunc("POST /sessions/{id}/run", func(w http.ResponseWriter, _ *http.Request) {
		// Emit a single assistant/message then close the stream without
		// emitting run.end. This is the failure mode the guard detects.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"type":"run.start","messageId":"m1"}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"type":"assistant/message","sessionId":"s1","data":{"message":{"content":[{"type":"text","text":"{\"outcome\":\"advance\"}"}]}}}`)
		if flusher != nil {
			flusher.Flush()
		}
		// Close without run.end.
	})
	b := dsh.NewBridge("http://127.0.0.1:1")
	b.SetHTTPClient(&http.Client{Timeout: time.Second})
	b.SetStreamHTTPClient(&http.Client{})
	// Spin up the test server on a real listener.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal(err) }
	addr := lis.Addr().String()
	go func() { _ = http.Serve(lis, mux) }()
	defer lis.Close()
	b = dsh.NewBridge("http://" + addr)
	evCh, errCh := b.Run(context.Background(), "s1", []dsh.ContentBlock{{Type: "text", Text: "go"}})
	var got int
	for range evCh { got++ }
	runErr := <-errCh
	if !errors.Is(runErr, dsh.ErrRunIncomplete) {
		t.Fatalf("err = %v, want ErrRunIncomplete", runErr)
	}
	if got == 0 {
		t.Errorf("expected the upstream assistant/message to be forwarded before the guard fires")
	}
}

func TestBridgeCreateSessionMissingFields(t *testing.T) {
	b := dsh.NewBridge("http://127.0.0.1:1")
	if _, err := b.CreateSession(context.Background(), dsh.CreateSessionRequest{}); err == nil {
		t.Error("expected missing-fields error")
	}
}

func TestBridgePrompt(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	ref, err := b.CreateSession(context.Background(), dsh.CreateSessionRequest{Cwd: "/x", Provider: "deepseek", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Prompt(context.Background(), ref.ID, []dsh.ContentBlock{{Type: "text", Text: "hi"}})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.MessageID != "msg-"+ref.ID {
		t.Errorf("messageId = %q, want msg-%s", res.MessageID, ref.ID)
	}
}

func TestBridgeCloseSessionReturnsNotSupported(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	ref, err := b.CreateSession(context.Background(), dsh.CreateSessionRequest{Cwd: "/x", Provider: "deepseek", Model: "deepseek-chat"})
	if err != nil {
		t.Fatal(err)
	}
	err = b.CloseSession(context.Background(), ref.ID)
	if err == nil {
		t.Fatal("expected ErrNotSupported")
	}
	if !errors.Is(err, dsh.ErrNotSupported) {
		t.Errorf("want ErrNotSupported, got %v", err)
	}
	if !strings.Contains(err.Error(), "upstream DSH") {
		t.Errorf("error message should mention upstream DSH: %v", err)
	}
}

func TestBridgeShutdown(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !fb.shutdownHit {
		t.Error("bridge did not call /shutdown")
	}
}

// TestBridgeSnapshotEventsReturnsNormalizedBatch verifies the legacy
// /events firehose path still works and Normalize routes upstream
// SessionEvent.type values to the right domain kinds. We pass through
// a script of upstream-shape events (no envelope wrap).
func TestBridgeSnapshotEventsReturnsNormalizedBatch(t *testing.T) {
	fb := newFakeBridge(t)
	fb.script = []dsh.RawBridgeEvent{
		{Type: "session.status", SessionID: "sess-x", Status: "running"},
		// Upstream SessionEvents are flattened — `type` is the upstream
		// SessionEvent.type, `data` carries the upstream `event.data`.
		{Type: "assistant/message", SessionID: "sess-x", Data: json.RawMessage(`{"text":"hello"}`)},
		{Type: "tool/call", SessionID: "sess-x", Data: json.RawMessage(`{"name":"read"}`)},
		{Type: "tool/result", SessionID: "sess-x", Data: json.RawMessage(`{"output":"x"}`)},
		{Type: "turn/end", SessionID: "sess-x"},
		{Type: "session.status", SessionID: "sess-x", Status: "idle"},
	}
	b := dsh.NewBridge(fb.srv.URL)
	batch, err := b.SnapshotEvents(context.Background(), "sess-x")
	if err != nil {
		t.Fatalf("SnapshotEvents: %v", err)
	}
	if len(batch) != 6 {
		t.Fatalf("batch = %d, want 6", len(batch))
	}
	want := []domain.EventKind{
		domain.EventKindAgentRunning,
		domain.EventKindRawPassthrough, // assistant/message
		domain.EventKindToolStarted,
		domain.EventKindToolCompleted,
		domain.EventKindRawPassthrough, // turn/end (NOT a synonym for activity end)
		domain.EventKindAgentIdle,
	}
	for i, ev := range batch {
		if ev.Kind != want[i] {
			t.Errorf("batch[%d].Kind = %q, want %q", i, ev.Kind, want[i])
		}
		if ev.SessionID != "sess-x" {
			t.Errorf("batch[%d].SessionID = %q", i, ev.SessionID)
		}
	}
	// The upstream `assistant/message` Data must survive Normalize.
	if !strings.Contains(string(batch[1].Data), `"text":"hello"`) {
		t.Errorf("batch[1].Data missing assistant message payload: %s", batch[1].Data)
	}
}

// TestBridgeRunReturnsBoundedBatchByRunEnd verifies the new Run
// path: subscribe-before-prompt (enforced by the bridge), idle-bound
// stream (closed by run.end{reason=idle}). No EOF dependency, no
// invented terminal event.
func TestBridgeRunReturnsBoundedBatchByRunEnd(t *testing.T) {
	fb := newFakeBridge(t)
	fb.runScripts["sess-deepseek-v3"] = []dsh.RawBridgeEvent{
		{Type: "agent/inbox/spliced", SessionID: "sess-deepseek-v3",
			Data: json.RawMessage(`{"inserted":[{"id":"msg-sess-deepseek-v3"}]}`)},
		{Type: "assistant/message", SessionID: "sess-deepseek-v3",
			Data: json.RawMessage(`{"text":"hello"}`)},
		{Type: "tool/call", SessionID: "sess-deepseek-v3",
			Data: json.RawMessage(`{"name":"read"}`)},
		{Type: "tool/result", SessionID: "sess-deepseek-v3",
			Data: json.RawMessage(`{"output":"x"}`)},
		{Type: "turn/end", SessionID: "sess-deepseek-v3"},
		{Type: "session.status", SessionID: "sess-deepseek-v3", Status: "idle"},
	}
	b := dsh.NewBridge(fb.srv.URL)
	evCh, errCh := b.Run(context.Background(), "sess-deepseek-v3",
		[]dsh.ContentBlock{{Type: "text", Text: "go"}})
	var collected []dsh.NormalizedEvent
	for ev := range evCh {
		collected = append(collected, ev)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("errCh: %v", err)
		}
	default:
	}
	if len(collected) < 6 {
		t.Fatalf("collected = %d, want at least 6", len(collected))
	}
	// First event is run.start, last is run.end.
	if collected[0].Kind != domain.EventKindRawPassthrough {
		t.Errorf("first event kind = %q, want raw.passthrough", collected[0].Kind)
	}
	if !strings.Contains(string(collected[0].Data), `"type":"run.start"`) {
		t.Errorf("first event data missing run.start: %s", collected[0].Data)
	}
	last := collected[len(collected)-1]
	if !strings.Contains(string(last.Data), `"type":"run.end"`) {
		t.Errorf("last event data missing run.end: %s", last.Data)
	}
	if !strings.Contains(string(last.Data), `"reason":"idle"`) {
		t.Errorf("last event reason != idle: %s", last.Data)
	}
}

// TestBridgeRunPropagatesTransportError verifies a bridge
// transport_error frame on the /run stream is routed to errCh without
// being misclassified as a domain raw.passthrough.
func TestBridgeRunPropagatesTransportError(t *testing.T) {
	// We reuse the legacy transport-error switch on /events; for /run
	// we need to script a transport_error + run.end. Override the
	// fakeBridge route by configuring emitTransportError and using the
	// /events path. The /run path test is in the bridge test
	// (runtime/dsh-bridge/tests/server.test.ts). For the Go side, we
	// use SnapshotEvents + a transport_error script.
	fb := newFakeBridge(t)
	fb.emitTransportError = "upstream_runtime_disconnected"
	b := dsh.NewBridge(fb.srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	evCh, errCh := b.Events(ctx, "sess-x")
	var gotErr error
	var gotEv dsh.NormalizedEvent
	var sawEv, sawErr bool
	for !(sawErr && sawEv || (sawErr && !sawEv)) {
		select {
		case gotEv, sawEv = <-evCh:
			if !sawEv {
				break
			}
		case gotErr, sawErr = <-errCh:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for transport error: errCh=%v evCh=%v", sawErr, sawEv)
		}
	}
	if gotErr == nil {
		t.Fatalf("errCh got nil; sawEv=%v gotEv=%+v", sawEv, gotEv)
	}
	if !strings.Contains(gotErr.Error(), "upstream_runtime_disconnected") {
		t.Errorf("error = %q, want upstream_runtime_disconnected substring", gotErr.Error())
	}
}

func TestBridgeEventsRoutesTransportErrorToErrCh(t *testing.T) {
	// Same as TestBridgeRunPropagatesTransportError but via the legacy
	// SnapshotEvents route (kept to verify reviewer P1 #2 / fhn).
	fb := newFakeBridge(t)
	fb.emitTransportError = "upstream_runtime_disconnected"
	b := dsh.NewBridge(fb.srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	evCh, errCh := b.Events(ctx, "sess-x")
	var gotErr error
	var gotEv dsh.NormalizedEvent
	var sawEv, sawErr bool
	for !(sawErr && sawEv || (sawErr && !sawEv)) {
		select {
		case gotEv, sawEv = <-evCh:
			if !sawEv {
				break
			}
		case gotErr, sawErr = <-errCh:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for transport error: errCh=%v evCh=%v", sawErr, sawEv)
		}
	}
	if gotErr == nil {
		t.Fatalf("errCh got nil; sawEv=%v gotEv=%+v", sawEv, gotEv)
	}
	if !strings.Contains(gotErr.Error(), "upstream_runtime_disconnected") {
		t.Errorf("error = %q, want upstream_runtime_disconnected substring", gotErr.Error())
	}
}

// TestNormalizeUnknownTypeBecomesPassthrough — unknown upstream `type`
// maps to EventKindRawPassthrough with the raw frame preserved.
func TestNormalizeUnknownTypeBecomesPassthrough(t *testing.T) {
	raw := &dsh.RawBridgeEvent{
		Type:      "some.unknown.upstream.type",
		SessionID: "sess-x",
		Data:      json.RawMessage(`{"hello":"world"}`),
	}
	ev := raw.Normalize(time.Now())
	if ev.Kind != domain.EventKindRawPassthrough {
		t.Errorf("kind = %q, want raw.passthrough", ev.Kind)
	}
	if !strings.Contains(string(ev.Data), `"hello":"world"`) {
		t.Errorf("Data should preserve raw payload: %s", ev.Data)
	}
}

func TestNormalizeNilReceiver(t *testing.T) {
	var raw *dsh.RawBridgeEvent
	ev := raw.Normalize(time.Now())
	if ev.Kind != domain.EventKindRawPassthrough {
		t.Errorf("kind = %q, want raw.passthrough", ev.Kind)
	}
}

func TestBridgeSnapshotEventsEmptySessionID(t *testing.T) {
	b := dsh.NewBridge("http://127.0.0.1:1")
	if _, err := b.SnapshotEvents(context.Background(), ""); err == nil {
		t.Fatal("expected missing-sessionID error")
	}
}

func TestBridgePromptEmptySessionID(t *testing.T) {
	b := dsh.NewBridge("http://127.0.0.1:1")
	if _, err := b.Prompt(context.Background(), "", nil); err == nil {
		t.Fatal("expected missing-sessionID error")
	}
}

func TestBridgeCloseSessionEmptySessionID(t *testing.T) {
	b := dsh.NewBridge("http://127.0.0.1:1")
	if err := b.CloseSession(context.Background(), ""); err == nil {
		t.Fatal("expected missing-sessionID error")
	}
}

func TestBridgeRunEmptySessionID(t *testing.T) {
	b := dsh.NewBridge("http://127.0.0.1:1")
	_, errCh := b.Run(context.Background(), "", nil)
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "sessionID required") {
			t.Errorf("err = %v", err)
		}
	default:
		t.Fatal("expected error on empty sessionID")
	}
}
