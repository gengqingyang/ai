package llm

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

const (
	openAIMaxRetries    = 2
	openAIRetryBackoff  = 500 * time.Millisecond
	openAIIdleConnLimit = 30 * time.Second
)

// newOpenAIHTTPClient avoids reusing connections that may have gone stale while
// an agent is waiting on a long-running tool, and retries failures that happen
// before a streaming response has started.
func newOpenAIHTTPClient() *http.Client {
	base := http.DefaultTransport
	if transport, ok := base.(*http.Transport); ok {
		transport = transport.Clone()
		transport.IdleConnTimeout = openAIIdleConnLimit
		base = transport
	}

	return &http.Client{Transport: &retryTransport{
		base:       base,
		maxRetries: openAIMaxRetries,
		backoff:    openAIRetryBackoff,
	}}
}

type retryTransport struct {
	base       http.RoundTripper
	maxRetries int
	backoff    time.Duration
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	for attempt := 0; ; attempt++ {
		attemptReq, err := requestForAttempt(req, attempt)
		if err != nil {
			return nil, err
		}

		resp, err := base.RoundTrip(attemptReq)
		if attempt >= t.maxRetries || !shouldRetryModelRequest(req, resp, err) {
			return resp, err
		}
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}

		delay := t.backoff * time.Duration(1<<attempt)
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		slog.Warn("模型请求暂时失败，准备重试",
			"attempt", attempt+1,
			"max_attempts", t.maxRetries+1,
			"status", status,
			"err", err,
			"backoff", delay,
		)

		if err := waitForRetry(req.Context(), delay); err != nil {
			return nil, err
		}
	}
}

func requestForAttempt(req *http.Request, attempt int) (*http.Request, error) {
	if attempt == 0 {
		return req, nil
	}
	retryReq := req.Clone(req.Context())
	if req.Body == nil {
		return retryReq, nil
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	retryReq.Body = body
	return retryReq, nil
}

func shouldRetryModelRequest(req *http.Request, resp *http.Response, err error) bool {
	if req.Context().Err() != nil {
		return false
	}
	if req.Body != nil && req.GetBody == nil {
		return false
	}
	if err != nil {
		// A transport error means no usable response headers were received. The
		// model call can be replayed without duplicating downstream tool actions.
		return true
	}
	if resp == nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusRequestTimeout,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ http.RoundTripper = (*retryTransport)(nil)
