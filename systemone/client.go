package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	TypeSafeBaseURL      = "https://api.typesafe.ai"
	TypeSafeDefaultModel = "jev-latest"

	CloudflareBaseURL      = "https://api.cloudflare.com/client/v4"
	CloudflareDefaultModel = "clef-flash"

	defaultTimeout     = 10 * time.Second
	defaultMaxAttempts = 3
	defaultBackoff     = 250 * time.Millisecond
)

// Client evaluates questions against a System One endpoint.
type Client struct {
	url         string
	token       string
	model       string
	http        *http.Client
	maxAttempts int
	backoff     time.Duration
	// enveloped marks responses wrapped in Cloudflare's REST envelope.
	enveloped bool
}

type options struct {
	model      string
	baseURL    string
	httpClient *http.Client
}

type Option func(*options)

// WithModel selects the model, e.g. "jev-latest" or "clef".
func WithModel(model string) Option { return func(o *options) { o.model = model } }

// WithBaseURL overrides the provider's API base URL.
func WithBaseURL(url string) Option { return func(o *options) { o.baseURL = url } }

func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.httpClient = c } }

// TypeSafe returns a client for TypeSafe's API, which serves Jev.
func TypeSafe(apiKey string, opts ...Option) *Client {
	o := resolve(opts, TypeSafeBaseURL, TypeSafeDefaultModel)
	return newClient(o, o.baseURL+"/v1/systemone", apiKey, false)
}

// Cloudflare returns a client for Cloudflare Workers AI, which serves Clef.
func Cloudflare(accountID, apiToken string, opts ...Option) *Client {
	o := resolve(opts, CloudflareBaseURL, CloudflareDefaultModel)
	return newClient(o, fmt.Sprintf("%s/accounts/%s/ai/run/@cf/cloudflare/%s", o.baseURL, accountID, o.model), apiToken, true)
}

func resolve(opts []Option, baseURL, model string) options {
	o := options{baseURL: baseURL, model: model, httpClient: &http.Client{Timeout: defaultTimeout}}
	for _, opt := range opts {
		opt(&o)
	}
	o.baseURL = strings.TrimSuffix(o.baseURL, "/")
	return o
}

func newClient(o options, url, token string, enveloped bool) *Client {
	return &Client{
		url:         url,
		token:       token,
		model:       o.model,
		http:        o.httpClient,
		maxAttempts: defaultMaxAttempts,
		backoff:     defaultBackoff,
		enveloped:   enveloped,
	}
}

// Model is the model requests are sent to.
func (c *Client) Model() string { return c.model }

// Endpoint is the URL requests are sent to.
func (c *Client) Endpoint() string { return c.url }

// Evaluate asks every question about state in one round trip. Every question
// is guaranteed an answer of its own kind.
func (c *Client) Evaluate(ctx context.Context, state any, questions map[string]Question) (*Response, error) {
	body, err := json.Marshal(request{Model: c.model, State: state, Questions: questions})
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}
	for attempt := 1; ; attempt++ {
		resp, err := c.post(ctx, body)
		if err == nil {
			if err := check(resp, questions); err != nil {
				return nil, err
			}
			return resp, nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable() || attempt == c.maxAttempts {
			return nil, err
		}
		if err := sleep(ctx, c.delay(attempt, apiErr.RetryAfter)); err != nil {
			return nil, err
		}
	}
}

func (c *Client) post(ctx context.Context, body []byte) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	return c.decode(httpResp, raw)
}

// envelope wraps every Cloudflare REST response.
type envelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
}

func (e envelope) message() string {
	msgs := make([]string, len(e.Errors))
	for i, x := range e.Errors {
		msgs[i] = fmt.Sprintf("%s (code %d)", x.Message, x.Code)
	}
	return strings.Join(msgs, "; ")
}

func (c *Client) decode(httpResp *http.Response, raw []byte) (*Response, error) {
	if httpResp.StatusCode/100 != 2 {
		apiErr := &APIError{Status: httpResp.StatusCode, Message: summarize(raw)}
		if s, err := strconv.Atoi(httpResp.Header.Get("Retry-After")); err == nil {
			apiErr.RetryAfter = time.Duration(s) * time.Second
		}
		return nil, apiErr
	}
	if c.enveloped {
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("decoding response envelope: %w: %s", err, summarize(raw))
		}
		if !env.Success {
			return nil, &APIError{Status: httpResp.StatusCode, Message: env.message()}
		}
		raw = env.Result
	}
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decoding response: %w: %s", err, summarize(raw))
	}
	return &resp, nil
}

func check(resp *Response, questions map[string]Question) error {
	for id, q := range questions {
		a, ok := resp.Answers[id]
		if !ok {
			return fmt.Errorf("response has no answer for question %q", id)
		}
		if a.Type != q.Kind() {
			return fmt.Errorf("question %q is a %s but its answer is a %s", id, q.Kind(), a.Type)
		}
	}
	return nil
}

func (c *Client) delay(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	d := c.backoff << (attempt - 1)
	return d/2 + rand.N(d/2+1)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

func summarize(raw []byte) string {
	const limit = 500
	s := strings.TrimSpace(string(raw))
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}

// APIError is a request the provider rejected.
type APIError struct {
	Status     int
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("system one API: %d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// Retryable reports whether the request may succeed if sent again: rate
// limits, TypeSafe's 529 Overloaded, and transient server errors.
func (e *APIError) Retryable() bool {
	switch e.Status {
	case http.StatusTooManyRequests, 529,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}
