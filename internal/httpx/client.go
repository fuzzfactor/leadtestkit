// Copyright 2026 Leadlight Authors
// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from leadlight api/client.go (commit 1f9d0e7), modified.

package httpx

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"leadtestkit/internal/logx"
)

const (
	defaultTimeout   = 120 * time.Second
	defaultAttempts  = 4
	defaultBaseDelay = 2 * time.Second
	defaultMaxDelay  = 60 * time.Second
	maxBodySize      = 64 << 20
	errorBodySnippet = 300
)

var UserAgent = "leadtestkit/0.1"

type transportMode int32

const (
	transportGo transportMode = iota
	transportCurl
	transportCurlAnon
)

func (m transportMode) String() string {
	switch m {
	case transportCurl:
		return "curl"
	case transportCurlAnon:
		return "curl-anon"
	}
	return "go"
}

type Options struct {
	Name     string
	MinDelay time.Duration
	Timeout  time.Duration

	// Attempts is the total number of tries for a request that fails
	// with a transient error.
	Attempts  int
	BaseDelay time.Duration
	MaxDelay  time.Duration

	// BotTiers enables escalation to curl when a JSON API answers with
	// a bot protection page.  CurlFirst is for HTML pages: curl is used
	// first and Go HTTP only if curl cannot be executed.
	BotTiers  bool
	CurlFirst bool

	Header http.Header

	// Authorization is only sent to AuthHost.
	Authorization string
	AuthHost      string

	HTTPClient *http.Client
	Logger     *slog.Logger
	Sleep      func(ctx context.Context, d time.Duration) error
}

type Client struct {
	opts      Options
	http      *http.Client
	log       *slog.Logger
	mu        sync.Mutex
	lastReq   time.Time
	transport atomic.Int32
}

type Request struct {
	Method string
	URL    string
	Body   []byte
	Header http.Header

	// NotFoundRetries treats 404 as transient this many times.  Used
	// for resources that are known to exist on overloaded servers.
	NotFoundRetries int

	// NoRetry disables retries for non-idempotent requests where a
	// duplicate would be harmful.
	NoRetry bool
}

type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	Via        string
}

type StatusError struct {
	Code int
	URL  string
	Body string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("HTTP %d: %s", e.Code, e.URL)
	}
	return fmt.Sprintf("HTTP %d: %s: %s", e.Code, e.URL, e.Body)
}

var ErrBotProtection = errors.New("blocked by bot protection")

func StatusCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

func IsNotFound(err error) bool { return StatusCode(err) == http.StatusNotFound }

func New(opts Options) *Client {
	if opts.Timeout == 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.Attempts == 0 {
		opts.Attempts = defaultAttempts
	}
	if opts.BaseDelay == 0 {
		opts.BaseDelay = defaultBaseDelay
	}
	if opts.MaxDelay == 0 {
		opts.MaxDelay = defaultMaxDelay
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	hc := opts.HTTPClient
	if hc == nil {
		// HTTP/2 TLS fingerprints get flagged by bot protection, and a
		// cookie jar would keep challenge cookies around.
		hc = &http.Client{
			Timeout: opts.Timeout,
			Transport: &http.Transport{
				Proxy:           http.ProxyFromEnvironment,
				TLSClientConfig: &tls.Config{NextProtos: []string{"http/1.1"}},
			},
		}
	}
	return &Client{opts: opts, http: hc, log: opts.Logger.With("client", opts.Name)}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// waitForRateLimit reserves the next request slot.  The mutex is never
// held while sleeping.
func (c *Client) waitForRateLimit(ctx context.Context) error {
	if c.opts.MinDelay <= 0 {
		return nil
	}
	for {
		c.mu.Lock()
		since := time.Since(c.lastReq)
		if c.lastReq.IsZero() || since >= c.opts.MinDelay {
			c.lastReq = time.Now()
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		c.log.Info(fmt.Sprintf("Rate limit: waiting %s before the next request.",
			(c.opts.MinDelay - since).Round(time.Millisecond)))
		if err := c.opts.Sleep(ctx, c.opts.MinDelay-since); err != nil {
			return err
		}
	}
}

// markRequestDone measures the delay from the end of the previous
// response, which is what matters to an overloaded server.
func (c *Client) markRequestDone() {
	c.mu.Lock()
	c.lastReq = time.Now()
	c.mu.Unlock()
}

func (c *Client) Get(ctx context.Context, rawURL string) (*Response, error) {
	return c.Do(ctx, Request{Method: http.MethodGet, URL: rawURL})
}

func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	if r.Method == "" {
		r.Method = http.MethodGet
	}
	notFound := 0
	var lastErr error
	for attempt := 1; ; attempt++ {
		resp, err := c.once(ctx, r)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var retryAfter time.Duration
		retry := false
		switch {
		case err != nil:
			lastErr = err
			retry = true
		case resp.StatusCode == http.StatusNotFound && notFound < r.NotFoundRetries:
			notFound++
			retry = true
			lastErr = c.statusError(r, resp)
		case isTransientStatus(resp):
			retry = true
			retryAfter = retryAfterDelay(resp.Header, time.Now())
			lastErr = c.statusError(r, resp)
		case resp.StatusCode >= 400:
			return resp, c.statusError(r, resp)
		default:
			return resp, nil
		}
		if !retry || r.NoRetry || attempt >= c.opts.Attempts {
			c.log.Warn(fmt.Sprintf("HTTP %s %s: not retrying after attempt %d: %v",
				r.Method, r.URL, attempt, lastErr))
			if resp != nil {
				return resp, lastErr
			}
			return nil, lastErr
		}
		delay := c.backoff(attempt)
		if retryAfter > delay {
			delay = retryAfter
		}
		if delay > c.opts.MaxDelay {
			if retryAfter > c.opts.MaxDelay {
				c.log.Warn(fmt.Sprintf("HTTP %s %s: wait %s exceeds the %s "+
					"per-request limit; deferring to a later retry.",
					r.Method, r.URL, retryAfter, c.opts.MaxDelay))
				err := fmt.Errorf("%w (asked to wait %s)", lastErr, retryAfter)
				return resp, err
			}
			delay = c.opts.MaxDelay
		}
		c.log.Warn(fmt.Sprintf("HTTP %s %s failed on attempt %d: %v. Retrying in %s.",
			r.Method, r.URL, attempt, lastErr, delay))
		if err := c.opts.Sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func (c *Client) backoff(attempt int) time.Duration {
	d := c.opts.BaseDelay << (attempt - 1)
	if d <= 0 || d > c.opts.MaxDelay {
		d = c.opts.MaxDelay
	}
	return d/2 + rand.N(d/2+1)
}

func isTransientStatus(resp *Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case http.StatusForbidden:
		// GitHub signals rate limiting with 403.
		if resp.Header.Get("Retry-After") != "" ||
			resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return true
		}
		return bytes.Contains(bytes.ToLower(resp.Body), []byte("secondary rate limit"))
	}
	return false
}

func retryAfterDelay(h http.Header, now time.Time) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return time.Duration(s) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			return t.Sub(now)
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if s, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return time.Unix(s, 0).Sub(now) + time.Second
		}
	}
	return 0
}

func (c *Client) statusError(r Request, resp *Response) error {
	body := strings.TrimSpace(string(resp.Body))
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		body = "(html page)"
	}
	if len(body) > errorBodySnippet {
		body = body[:errorBodySnippet] + "..."
	}
	return &StatusError{Code: resp.StatusCode, URL: r.URL, Body: logx.Redact(body)}
}

func (c *Client) newRequest(ctx context.Context, r Request) (*http.Request, error) {
	var body io.Reader
	if r.Body != nil {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "*/*")
	for k, v := range c.opts.Header {
		req.Header[k] = v
	}
	for k, v := range r.Header {
		req.Header[k] = v
	}
	if r.Body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.opts.Authorization != "" && sameHost(req.URL, c.opts.AuthHost) {
		req.Header.Set("Authorization", c.opts.Authorization)
	}
	return req, nil
}

func sameHost(u *url.URL, host string) bool {
	return host != "" && strings.EqualFold(u.Hostname(), host)
}

func (c *Client) once(ctx context.Context, r Request) (*Response, error) {
	if err := c.waitForRateLimit(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if c.opts.MinDelay > 0 {
			c.markRequestDone()
		}
	}()
	if c.opts.CurlFirst {
		return c.onceCurlFirst(ctx, r)
	}

	mode := transportMode(c.transport.Load())
	if mode != transportGo {
		return c.viaCurl(ctx, r, mode)
	}
	resp, err := c.viaGo(ctx, r)
	if err != nil || !c.opts.BotTiers || !isBotResponse(resp) {
		return resp, err
	}

	// A different TLS fingerprint (curl) usually passes; if not, curl's
	// default User-Agent is accepted by most bot filters.
	c.log.Warn(fmt.Sprintf("Bot protection blocked %s; retrying with curl.", r.URL))
	for _, m := range []transportMode{transportCurl, transportCurlAnon} {
		cr, cerr := c.viaCurl(ctx, r, m)
		if cerr == nil && !isBotResponse(cr) {
			c.transport.Store(int32(m))
			c.log.Warn(fmt.Sprintf("Using %s after bypassing bot protection.",
				m.String()))
			return cr, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrBotProtection, r.URL)
}

func (c *Client) onceCurlFirst(ctx context.Context, r Request) (*Response, error) {
	resp, err := c.viaCurl(ctx, r, transportCurl)
	if err != nil && !curlAvailable() {
		c.log.Warn("Curl is unavailable; falling back to Go's HTTP client.")
		resp, err = c.viaGo(ctx, r)
	}
	if err == nil && looksLikeChallenge(resp.Body) {
		return nil, fmt.Errorf("%w: %s", ErrBotProtection, r.URL)
	}
	return resp, err
}

func (c *Client) viaGo(ctx context.Context, r Request) (*Response, error) {
	req, err := c.newRequest(ctx, r)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	c.log.Info(fmt.Sprintf("HTTP %s %s: sending request using Go.", r.Method, r.URL))
	hr, err := c.http.Do(req)
	if err != nil {
		c.log.Warn(fmt.Sprintf("HTTP %s %s failed after %s using Go: %v",
			r.Method, r.URL, time.Since(start).Round(time.Millisecond), err))
		return nil, err
	}
	defer hr.Body.Close()
	body, err := io.ReadAll(io.LimitReader(hr.Body, maxBodySize))
	if err != nil {
		c.log.Warn(fmt.Sprintf("HTTP %s %s: response read failed after %s using Go: %v",
			r.Method, r.URL, time.Since(start).Round(time.Millisecond), err))
		return nil, err
	}
	c.log.Info(fmt.Sprintf("HTTP %s %s: received %d %s in %s using Go.",
		r.Method, r.URL, hr.StatusCode, http.StatusText(hr.StatusCode),
		time.Since(start).Round(time.Millisecond)))
	return &Response{StatusCode: hr.StatusCode, Header: hr.Header, Body: body, Via: "go"}, nil
}

func (c *Client) viaCurl(ctx context.Context, r Request, mode transportMode) (*Response, error) {
	req, err := c.newRequest(ctx, r)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	c.log.Info(fmt.Sprintf("HTTP %s %s: sending request using %s.",
		r.Method, r.URL, mode.String()))
	hr, err := execCurl(ctx, req, r.Body, mode == transportCurlAnon, c.opts.Timeout)
	if err != nil {
		c.log.Warn(fmt.Sprintf("HTTP %s %s failed after %s using %s: %v",
			r.Method, r.URL, time.Since(start).Round(time.Millisecond),
			mode.String(), err))
		return nil, err
	}
	body, _ := io.ReadAll(hr.Body)
	c.log.Info(fmt.Sprintf("HTTP %s %s: received %d %s in %s using %s.",
		r.Method, r.URL, hr.StatusCode, http.StatusText(hr.StatusCode),
		time.Since(start).Round(time.Millisecond), mode.String()))
	return &Response{StatusCode: hr.StatusCode, Header: hr.Header, Body: body,
		Via: mode.String()}, nil
}

// isBotResponse detects bot protection pages served instead of JSON.
// Other status codes with HTML are server errors, not bot protection.
func isBotResponse(resp *Response) bool {
	switch resp.StatusCode {
	case 200, 403, 503:
		return strings.Contains(resp.Header.Get("Content-Type"), "text/html")
	}
	return false
}

func looksLikeChallenge(body []byte) bool {
	head := body
	if len(head) > 16384 {
		head = head[:16384]
	}
	low := bytes.ToLower(head)
	return bytes.Contains(low, []byte("not a bot")) ||
		bytes.Contains(low, []byte("anubis_challenge")) ||
		bytes.Contains(low, []byte("id=\"anubis"))
}
