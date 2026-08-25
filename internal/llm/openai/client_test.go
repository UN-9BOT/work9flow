package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChatCompletionsHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		var req Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "MiniMax-M3" || len(req.Messages) != 1 {
			t.Errorf("req = %+v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{"role": "assistant", "content": "ok"},
			}},
		})
	}))
	defer srv.Close()

	c, err := New(srv.URL, "", "k")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.ChatCompletions(context.Background(), Request{
		Model:    "MiniMax-M3",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "ok" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestChatCompletionsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "", "k")
	_, err := c.ChatCompletions(context.Background(), Request{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "x"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestChatCompletionsRejectsEmpty(t *testing.T) {
	c, _ := New("http://x", "", "k")
	if _, err := c.ChatCompletions(context.Background(), Request{}); err == nil {
		t.Error("empty request: expected error")
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv("W9F_TEST_KEY", "secret")
	c, err := New("http://x", "W9F_TEST_KEY", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "secret" {
		t.Errorf("apiKey = %q", c.APIKey)
	}
	if _, err := New("http://x", "W9F_TEST_KEY_MISSING", ""); err == nil {
		t.Error("missing env: expected error")
	}
}
