// Package localdsh is a tiny DSH-compatible HTTP server that talks to
// one OpenAI-compatible provider. It exists so work9flow can drive a
// real feature-development workflow end-to-end without an external DSH
// Node process — useful for local smoke testing and as a reference for
// future providers.
//
// Wire compatibility:
//
//	POST /v1/sessions                 {role, model}        -> {id}
//	POST /v1/sessions/{id}/followup   {message, data}      -> 204
//	POST /v1/sessions/{id}/steer      {message, data}      -> 204
//	POST /v1/sessions/{id}/cancel                           -> 204
//	GET  /v1/sessions/{id}/events                          -> ndjson event stream
//	GET  /v1/health                                          -> {status:"ok"}
//
// When /events is first polled after a followup, the server kicks off
// a single OpenAI chat completion (model = session.Model, role prompt
// derived from role + the stored followup message) and emits one
// `agent.completed` event whose data contains
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
	Role      string
	Model     string
	CreatedAt time.Time

	mu          sync.Mutex
	lastMessage string
	completed   bool
	canceled    bool
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
	mux.HandleFunc("/v1/health", s.health)
	mux.HandleFunc("/v1/sessions", s.createSession)
	mux.HandleFunc("/v1/sessions/", s.sessionSub)
	return mux
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Role  string `json:"role"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if body.Role == "" {
		http.Error(w, "role required", http.StatusBadRequest)
		return
	}
	sess := &Session{
		ID:        "sess-" + uuid.NewString(),
		Role:      body.Role,
		Model:     firstNonEmpty(body.Model, s.Provider.Model),
		CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]string{"id": sess.ID})
}

func (s *Server) sessionSub(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	id, op := parts[0], parts[1]
	s.mu.Lock()
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}
	switch op {
	case "followup":
		s.followup(w, r, sess)
	case "steer":
		s.steer(w, r, sess)
	case "cancel":
		s.cancel(w, r, sess)
	case "events":
		s.events(w, r, sess)
	default:
		http.Error(w, "unknown op", http.StatusNotFound)
	}
}

func (s *Server) followup(w http.ResponseWriter, r *http.Request, sess *Session) {
	var body struct {
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	sess.mu.Lock()
	sess.lastMessage = body.Message
	sess.completed = false
	sess.completion = nil
	sess.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) steer(w http.ResponseWriter, _ *http.Request, sess *Session) {
	sess.mu.Lock()
	sess.lastMessage += "\n[steer]"
	sess.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) cancel(w http.ResponseWriter, _ *http.Request, sess *Session) {
	sess.mu.Lock()
	sess.canceled = true
	sess.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// events streams the session's normalized event log. When the session
// has not yet completed, this call kicks off the OpenAI request and
// blocks (up to 25s) until the model responds or the session is
// canceled. The response is a single agent.completed ndjson line.
func (s *Server) events(w http.ResponseWriter, r *http.Request, sess *Session) {
	sess.mu.Lock()
	canceled := sess.canceled
	last := sess.lastMessage
	done := sess.completed
	sess.mu.Unlock()

	if canceled {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": sess.ID,
			"kind":       "session.canceled",
			"at":         time.Now().UTC(),
		})
		return
	}
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
	w.Header().Set("Content-Type", "application/x-ndjson")
	payload := map[string]any{
		"outcome": c.outcome,
		"summary": c.summary,
		"raw":     c.raw,
	}
	if c.err != "" {
		payload["error"] = c.err
		payload["outcome"] = "failed"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"session_id": sess.ID,
		"kind":       "agent.completed",
		"at":         time.Now().UTC(),
		"data":       payload,
	})
}

func (s *Server) runCompletion(sess *Session, prompt string) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	c, err := openai.New(s.Provider.BaseURL, "", s.Provider.APIKey)
	if err != nil {
		s.completeWithError(sess, err)
		return
	}
	sys := roleSystemPrompt(sess.Role)
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
