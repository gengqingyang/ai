package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"diagnostic-system/internal/config"
)

func TestOpenAIChatModelRequest(t *testing.T) {
	const (
		token = "test-token"
		model = "gpt-5.6-sol"
	)

	var gotBody map[string]any
	attempts := 0
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("request path = %q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q, want Bearer token", got)
		}
		if attempts == 1 {
			return nil, io.EOF
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}

		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":1,
			"model":"gpt-5.6-sol",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]
		}`)),
			Request: r,
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })

	cm, err := NewChatModel(context.Background(), &config.Config{
		Provider:    "openai",
		BaseURL:     "https://api.qiso.io",
		AuthToken:   token,
		Model:       model,
		MaxTokens:   4096,
		Temperature: -1,
	})
	if err != nil {
		t.Fatalf("NewChatModel() error = %v", err)
	}

	msg, err := cm.Generate(context.Background(), []*schema.Message{schema.UserMessage("hello")})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if msg.Content != "ok" {
		t.Errorf("response content = %q, want ok", msg.Content)
	}
	if attempts != 2 {
		t.Errorf("request attempts = %d, want 2", attempts)
	}
	if gotBody["model"] != model {
		t.Errorf("request model = %v, want %s", gotBody["model"], model)
	}
	if gotBody["max_completion_tokens"] != float64(4096) {
		t.Errorf("max_completion_tokens = %v, want 4096", gotBody["max_completion_tokens"])
	}
	if _, exists := gotBody["max_tokens"]; exists {
		t.Error("GPT 请求不应发送已废弃的 max_tokens")
	}
}

func TestRetryTransportDoesNotRetryBadRequest(t *testing.T) {
	attempts := 0
	transport := &retryTransport{
		base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Body:       io.NopCloser(strings.NewReader("bad request")),
				Request:    r,
			}, nil
		}),
		maxRetries: 2,
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://api.qiso.io/v1/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	defer resp.Body.Close()
	if attempts != 1 {
		t.Errorf("request attempts = %d, want 1", attempts)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestOpenAIBaseURL(t *testing.T) {
	tests := map[string]string{
		"":                          "",
		"https://api.openai.com/v1": "https://api.openai.com/v1",
		"https://api.qiso.io":       "https://api.qiso.io/v1",
		"https://api.qiso.io/":      "https://api.qiso.io/v1",
	}
	for input, want := range tests {
		if got := openAIBaseURL(input); got != want {
			t.Errorf("openAIBaseURL(%q) = %q, want %q", input, got, want)
		}
	}
}
