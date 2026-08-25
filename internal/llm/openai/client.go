// Package openai is a minimal OpenAI-compatible chat completions client.
// It targets the /v1/chat/completions endpoint used by OpenAI and any
// provider that speaks the same wire format. The client is intentionally
// small: work9flow uses it via internal/llm/localdsh to drive a single
// role-specific session per workflow stage.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client talks to one OpenAI-compatible provider.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New returns a Client. apiKey is read from the named environment
// variable; an empty envName means the key is taken from the envName
// string itself (useful for tests).
func New(baseURL, envName, apiKey string) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("openai: base_url required")
	}
	key := apiKey
	if envName != "" && key == "" {
		key = os.Getenv(envName)
	}
	if envName != "" && key == "" {
		return nil, fmt.Errorf("openai: env %s is empty", envName)
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  key,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Message is one turn in a chat completion request.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is the body posted to /v1/chat/completions.
type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream,omitempty"`
}

// Choice is one response option.
type Choice struct {
	Message Message `json:"message"`
}

// Response is the parsed body of /v1/chat/completions.
type Response struct {
	Choices []Choice `json:"choices"`
}

// ChatCompletions issues a non-streaming chat completion and returns
// the first choice's message content.
func (c *Client) ChatCompletions(ctx context.Context, req Request) (Response, error) {
	if req.Model == "" {
		return Response{}, errors.New("openai: model required")
	}
	if len(req.Messages) == 0 {
		return Response{}, errors.New("openai: messages required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("openai: http: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}
	if resp.StatusCode/100 != 2 {
		return Response{}, fmt.Errorf("openai: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{}, fmt.Errorf("openai: decode: %w (body=%s)", err, truncate(string(raw), 256))
	}
	if len(out.Choices) == 0 {
		return Response{}, fmt.Errorf("openai: empty choices in response")
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
