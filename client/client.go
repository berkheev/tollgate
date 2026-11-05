// Package client is a small HTTP client for tollgate, plus a net/http
// middleware that rate limits incoming requests through it.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	http    *http.Client
}

type Option func(*Client)

// WithHTTPClient replaces the default client (2s timeout, pooled transport).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		h := *c.http
		h.Timeout = d
		c.http = &h
	}
}

func New(baseURL string, opts ...Option) *Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 64
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 2 * time.Second, Transport: t},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type Decision struct {
	Allowed    bool
	Limit      int64
	Remaining  int64
	ResetAfter time.Duration
	RetryAfter time.Duration
	// Degraded means tollgate could not reach its backend and answered
	// according to its failure mode.
	Degraded bool
	// Policy is the RateLimit-Policy header, e.g. "100;w=60;burst=20".
	Policy string
}

type Rule struct {
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	Limit     int64  `json:"limit"`
	Window    string `json:"window"`
	WindowMs  int64  `json:"window_ms"`
	Burst     int64  `json:"burst,omitempty"`
	Capacity  int64  `json:"capacity"`
	Spec      string `json:"spec"`
}

// APIError is returned for responses other than 2xx and 429.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("tollgate: %d: %s", e.StatusCode, e.Message)
}

// Check consumes one unit of rule for key.
func (c *Client) Check(ctx context.Context, rule, key string) (Decision, error) {
	return c.CheckN(ctx, rule, key, 1)
}

func (c *Client) CheckN(ctx context.Context, rule, key string, cost int64) (Decision, error) {
	payload := map[string]any{"rule": rule, "key": key, "cost": cost}
	resp, err := c.do(ctx, http.MethodPost, "/v1/check", payload)
	if err != nil {
		return Decision{}, err
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTooManyRequests {
		return Decision{}, apiError(resp)
	}
	var body struct {
		Allowed      bool  `json:"allowed"`
		Limit        int64 `json:"limit"`
		Remaining    int64 `json:"remaining"`
		ResetAfterMs int64 `json:"reset_after_ms"`
		RetryAfterMs int64 `json:"retry_after_ms"`
		Degraded     bool  `json:"degraded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Decision{}, fmt.Errorf("tollgate: decode response: %w", err)
	}
	return Decision{
		Allowed:    body.Allowed,
		Limit:      body.Limit,
		Remaining:  body.Remaining,
		ResetAfter: time.Duration(body.ResetAfterMs) * time.Millisecond,
		RetryAfter: time.Duration(body.RetryAfterMs) * time.Millisecond,
		Degraded:   body.Degraded,
		Policy:     resp.Header.Get("RateLimit-Policy"),
	}, nil
}

func (c *Client) Reset(ctx context.Context, rule, key string) error {
	resp, err := c.do(ctx, http.MethodPost, "/v1/reset", map[string]string{"rule": rule, "key": key})
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent {
		return apiError(resp)
	}
	return nil
}

func (c *Client) Rules(ctx context.Context) ([]Rule, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/rules", nil)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var body struct {
		Rules []Rule `json:"rules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("tollgate: decode response: %w", err)
	}
	return body.Rules, nil
}

func (c *Client) do(ctx context.Context, method, path string, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tollgate: %w", err)
	}
	return resp, nil
}

func apiError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if json.Unmarshal(raw, &body) != nil || body.Error == "" {
		body.Error = strings.TrimSpace(string(raw))
	}
	return &APIError{StatusCode: resp.StatusCode, Message: body.Error}
}

// drain lets the transport reuse the connection.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
