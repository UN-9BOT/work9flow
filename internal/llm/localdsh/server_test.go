package localdsh

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	resp, err := http.Post(srv.URL+"/v1/sessions", "application/json",
		strings.NewReader(`{"role":"scout","model":"fake-model"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created struct{ ID string `json:"id"` }
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if created.ID == "" {
		t.Fatal("no session id")
	}

	// followup
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/sessions/"+created.ID+"/followup",
		strings.NewReader(`{"message":"hi","data":null}`))
	if resp, err := http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}

	// events
	r, err := http.Get(srv.URL + "/v1/sessions/" + created.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(r.Body)
	if !scanner.Scan() {
		t.Fatal("no events")
	}
	var ev map[string]any
	if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	if ev["kind"] != "agent.completed" {
		t.Errorf("kind = %v", ev["kind"])
	}
	data, _ := ev["data"].(map[string]any)
	if data["outcome"] != "advance" {
		t.Errorf("outcome = %v", data["outcome"])
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

var _ = time.Second // keep time import if not used
