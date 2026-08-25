// Package localdsh is a tiny DSH-compatible HTTP server that talks to
// one OpenAI-compatible provider. It exists so work9flow can drive a
// real feature-development workflow end-to-end without an external DSH
// Node process — useful for local smoke testing and as a reference for
// future providers.
//
// Wire compatibility (mirrors runtime/dsh-bridge HTTP surface):
//
//	POST /sessions                       {cwd, provider, model, maxTokens?} -> {sessionId, serverInfo}
//	POST /sessions/{id}/prompt           {contentBlocks}                   -> {messageId}
//	POST /sessions/{id}/close                                                 -> 501 not_supported
//	POST /shutdown                                                           -> 204
//	GET  /sessions/{id}/events                                              -> SSE stream of session.event/session.status/subagent.* frames
//	GET  /health                                                             -> {status, serverInfo, message}
//
// When /events is opened after a prompt, the server kicks off
// a single OpenAI chat completion (model = session.Model, role prompt
// derived from role + the stored followup message) and emits one
// session.event with kind=agent.completed and data containing
//
//	{"outcome":"<parsed>","summary":"<model text>","raw":"<model text>"}
//
// Outcome parsing is best-effort: if the model text contains a JSON
// object with an "outcome" field, that wins; otherwise the server
// emits "advance".
package localdsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/unbot/work9flow/internal/llm/openai"
)

// Provider is the minimum the server needs from a provider definition.
type Provider struct {
	BaseURL   string
	APIKeyEnv string
	APIKey    string // optional explicit key; wins over APIKeyEnv
	Model     string // the default model id to use when session.Model is empty
}

// Session is one in-flight role run.
type Session struct {
	ID        string
	Cwd       string
	Provider  string
	Model     string
	CreatedAt time.Time

	mu          sync.Mutex
	lastMessage string
	completed   bool
	completion  *completion
}

// completion caches the result of one OpenAI call.
type completion struct {
	outcome string
	summary string
	raw     string
	err     string
}

// Server is the HTTP handler.
type Server struct {
	Provider Provider

	mu       sync.Mutex
	sessions map[string]*Session
}

// New returns a Server bound to provider. If provider.APIKey is empty
// and provider.APIKeyEnv is set, the env var is read once at startup.
func New(provider Provider) (*Server, error) {
	if provider.BaseURL == "" {
		return nil, errors.New("localdsh: base_url required")
	}
	if provider.APIKey == "" && provider.APIKeyEnv != "" {
		v := os.Getenv(provider.APIKeyEnv)
		if v == "" {
			return nil, fmt.Errorf("localdsh: env %s is empty", provider.APIKeyEnv)
		}
		provider.APIKey = v
	}
	if provider.Model == "" {
		provider.Model = "default"
	}
	return &Server{
		Provider: provider,
		sessions: map[string]*Session{},
	}, nil
}

// Handler returns the HTTP handler for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /sessions", s.handleCreateSession)
	mux.HandleFunc("POST /sessions/{id}/prompt", s.handlePrompt)
	mux.HandleFunc("POST /sessions/{id}/close", s.handleClose)
	mux.HandleFunc("GET /sessions/{id}/events", s.handleEvents)
	mux.HandleFunc("POST /shutdown", s.handleShutdown)
	return mux
}

func (s *Server) lookup(id string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ready","serverInfo":{"name":"localdsh","version":"test"}}`))
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cwd       string `json:"cwd"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		MaxTokens *int   `json:"maxTokens,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Provider == "" {
		http.Error(w, "provider required", http.StatusBadRequest)
		return
	}
	if body.Model == "" {
		body.Model = s.Provider.Model
	}
	if body.Cwd == "" {
		body.Cwd = "/"
	}
	sess := &Session{
		ID:        "sess-" + uuid.NewString(),
		Cwd:       body.Cwd,
		Provider:  body.Provider,
		Model:     body.Model,
		CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sessionId":  sess.ID,
		"serverInfo": map[string]string{"name": "localdsh", "version": "test"},
	})
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.lookup(id)
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	var body struct {
		ContentBlocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"contentBlocks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	var msg string
	for _, b := range body.ContentBlocks {
		if b.Type == "text" {
			msg += b.Text
		}
	}
	sess.mu.Lock()
	sess.lastMessage = msg
	sess.completed = false
	sess.completion = nil
	sess.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"messageId": "msg-" + sess.ID})
}

func (s *Server) handleClose(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  "not_supported",
		"detail": "upstream DSH SDK has no per-session close; shutdown the runtime instead",
	})
}

func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

// handleEvents streams the session's normalized event log. When the
// session has not yet completed, this call kicks off the OpenAI request
// and blocks (up to 25s) until the model responds. The response is a
// single session.event with kind=agent.completed, followed by
// session.status=idle.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.lookup(id)
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	sess.mu.Lock()
	last := sess.lastMessage
	done := sess.completed
	sess.mu.Unlock()

	if !done {
		s.runCompletion(sess, last)
	}
	sess.mu.Lock()
	c := sess.completion
	sess.mu.Unlock()
	if c == nil {
		http.Error(w, "no completion", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	payload := map[string]any{
		"outcome": c.outcome,
		"summary": c.summary,
		"raw":     c.raw,
	}
	if c.err != "" {
		payload["error"] = c.err
		payload["outcome"] = "failed"
	}
	eventJSON, _ := json.Marshal(map[string]any{
		"kind":      "session.event",
		"sessionId": sess.ID,
		"event": map[string]any{
			"kind": "agent.completed",
			"data": payload,
		},
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", eventJSON)
	statusJSON, _ := json.Marshal(map[string]any{
		"kind":      "session.status",
		"sessionId": sess.ID,
		"status":    "idle",
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", statusJSON)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) runCompletion(sess *Session, prompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	c, err := openai.New(s.Provider.BaseURL, "", s.Provider.APIKey)
	if err != nil {
		s.completeWithError(sess, err)
		return
	}
	sys := roleSystemPrompt(sess.Provider)
	resp, err := c.ChatCompletions(ctx, openai.Request{
		Model: sess.Model,
		Messages: []openai.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: strings.TrimSpace(prompt)},
		},
	})
	if err != nil {
		s.completeWithError(sess, err)
		return
	}
	if len(resp.Choices) == 0 {
		s.completeWithError(sess, errors.New("empty choices"))
		return
	}
	text := resp.Choices[0].Message.Content
	sess.mu.Lock()
	sess.completion = &completion{
		outcome: parseOutcome(text),
		summary: oneLineSummary(text),
		raw:     text,
	}
	sess.completed = true
	sess.mu.Unlock()
}

func (s *Server) completeWithError(sess *Session, err error) {
	sess.mu.Lock()
	sess.completion = &completion{
		outcome: "failed",
		err:     err.Error(),
		raw:     err.Error(),
	}
	sess.completed = true
	sess.mu.Unlock()
}

// roleSystemPrompt is a tiny instruction telling the model how to
// emit its outcome. Real DSH would inject a much richer per-role
// system prompt; this is the MVP.
func roleSystemPrompt(role string) string {
	return fmt.Sprintf(
		"You are the work9flow %s agent. "+
			"Reply with a brief plain-text summary of what you would do, then on the last line emit "+
			"a JSON object with an \"outcome\" field. "+
			"Recognised outcomes: advance | approve | revise | revise_plan | wait_user | done | failed | blocked_by_plan. "+
			"If unsure, use advance.",
		role)
}

var outcomeRE = regexp.MustCompile(`(?i)\boutcome\s*["'=:]*\s*([a-z_]+)`)

// parseOutcome finds an outcome in the model's text. Falls back to
// "advance" if no recognisable token is found.
func parseOutcome(text string) string {
	m := outcomeRE.FindStringSubmatch(text)
	if len(m) < 2 {
		return "advance"
	}
	switch strings.ToLower(m[1]) {
	case "advance", "approve", "revise", "revise_plan", "wait_user", "done", "failed", "blocked_by_plan":
		return strings.ToLower(m[1])
	}
	return "advance"
}

func oneLineSummary(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.Index(text, "\n"); i >= 0 {
		text = text[:i]
	}
	if len(text) > 240 {
		text = text[:240] + "..."
	}
	return text
}
