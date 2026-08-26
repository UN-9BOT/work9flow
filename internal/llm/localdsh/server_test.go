package localdsh

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unbot/work9flow/internal/llm/openai"
)

// fakeOpenAI returns canned chat completion responses. The "model"
// header isn't relevant here.
func fakeOpenAI(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{"role": "assistant", "content": reply},
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newServerWithFake(t *testing.T, reply string) (*httptest.Server, *Server) {
	t.Helper()
	fake := fakeOpenAI(t, reply)
	s, err := New(Provider{
		BaseURL: fake.URL,
		APIKey:  "test-key",
		Model:   "fake-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, s
}

func TestServerSessionLifecycle(t *testing.T) {
	srv, _ := newServerWithFake(t, "I scouted the repo. outcome: advance")

	// create session
	resp, err := http.Post(srv.URL+"/sessions", "application/json",
		strings.NewReader(`{"cwd":"/tmp","provider":"fake","model":"fake-model"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.SessionID == "" {
		t.Fatal("no session id")
	}

	// prompt
	promptBody := strings.NewReader(`{"contentBlocks":[{"type":"text","text":"hi"}]}`)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/sessions/"+created.SessionID+"/prompt", promptBody)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}

	// events
	r, err := http.Get(srv.URL + "/sessions/" + created.SessionID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(r.Body)
	var foundAgentCompleted bool
	var foundIdle bool
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatal(err)
		}
		if ev["kind"] == "session.event" {
			inner, _ := ev["event"].(map[string]any)
			if inner != nil && inner["kind"] == "agent.completed" {
				foundAgentCompleted = true
				data, _ := inner["data"].(map[string]any)
				if data == nil || data["outcome"] != "advance" {
					t.Errorf("agent.completed outcome = %v", data)
				}
			}
		}
		if ev["kind"] == "session.status" && ev["status"] == "idle" {
			foundIdle = true
		}
	}
	if !foundAgentCompleted {
		t.Error("no session.event agent.completed frame")
	}
	if !foundIdle {
		t.Error("no session.status=idle frame")
	}
}

func TestServerHealth(t *testing.T) {
	srv, _ := newServerWithFake(t, "ok")
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ready" {
		t.Errorf("status = %v", body["status"])
	}
}

func TestServerCloseSessionReturns501(t *testing.T) {
	srv, _ := newServerWithFake(t, "ok")
	// mint a session first
	resp, _ := http.Post(srv.URL+"/sessions", "application/json",
		strings.NewReader(`{"cwd":"/tmp","provider":"fake","model":"m"}`))
	var c struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&c)
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/sessions/"+c.SessionID+"/close", nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", r.StatusCode)
	}
}

func TestServerShutdownReturns204(t *testing.T) {
	srv, _ := newServerWithFake(t, "ok")
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/shutdown", nil)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", r.StatusCode)
	}
}

func TestParseOutcome(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hi\noutcome: approve", "approve"},
		{"{\"outcome\":\"revise\"}", "revise"},
		{"outcome = wait_user\nbye", "wait_user"},
		{"nothing here", "advance"},
		{"outcome: bogus_value", "advance"},
	}
	for _, c := range cases {
		if got := parseOutcome(c.in); got != c.want {
			t.Errorf("parseOutcome(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestNewRequiresAPIKey(t *testing.T) {
	if _, err := New(Provider{BaseURL: "http://x", APIKeyEnv: "W9F_NO_SUCH_ENV_XYZ"}); err == nil {
		t.Error("expected env error")
	}
}

func TestRoleSystemPromptMentionsRole(t *testing.T) {
	p := roleSystemPrompt("reviewer")
	if !strings.Contains(p, "reviewer") {
		t.Errorf("prompt missing role: %s", p)
	}
	if !strings.Contains(p, "outcome") {
		t.Errorf("prompt missing outcome field: %s", p)
	}
}

func TestOneLineSummaryTruncates(t *testing.T) {
	long := strings.Repeat("a", 500)
	got := oneLineSummary(long)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("no ellipsis: %q", got)
	}
	if len(got) > 250 {
		t.Errorf("too long: %d", len(got))
	}
}

func TestOpenAIIntegrationWithLocalDSH(t *testing.T) {
	// Sanity check: real openai.Client can hit the fake server.
	srv := fakeOpenAI(t, "ok")
	c, _ := openai.New(srv.URL, "", "k")
	resp, err := c.ChatCompletions(t.Context(), openai.Request{
		Model:    "m",
		Messages: []openai.Message{{Role: "user", Content: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Errorf("got %+v", resp)
	}
}
