// Copyright 2026 Leadlight Authors
// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from leadlight api/client_test.go (commit 1f9d0e7), modified.

package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func noSleep(ctx context.Context, d time.Duration) error { return ctx.Err() }

func testClient(t *testing.T, h http.HandlerFunc, opts Options) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	if opts.HTTPClient == nil {
		opts.HTTPClient = srv.Client()
	}
	if opts.Sleep == nil {
		opts.Sleep = noSleep
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return New(opts), srv
}

func TestRateLimit(t *testing.T) {
	var count atomic.Int32
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.Write([]byte(`{}`))
	}, Options{MinDelay: 50 * time.Millisecond, Sleep: sleepCtx})

	start := time.Now()
	c.Get(context.Background(), srv.URL+"/first")
	c.Get(context.Background(), srv.URL+"/second")
	if count.Load() != 2 {
		t.Errorf("count = %d, want 2", count.Load())
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Errorf("rate limit not applied")
	}
}

func TestRateLimitHonorsContext(t *testing.T) {
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}, Options{MinDelay: time.Hour, Sleep: sleepCtx})
	c.Get(context.Background(), srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Get(ctx, srv.URL); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}

func TestAuthorizationOnlyForAuthHost(t *testing.T) {
	var got []string
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Write([]byte(`{}`))
	}, Options{Authorization: "Token lorem"})
	u, _ := url.Parse(srv.URL)
	c.opts.AuthHost = u.Hostname()
	c.Get(context.Background(), srv.URL)
	c.opts.AuthHost = "ipsum.example"
	c.Get(context.Background(), srv.URL)
	if got[0] != "Token lorem" || got[1] != "" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestUserAgentAndHeaders(t *testing.T) {
	var ua, accept string
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		accept = r.Header.Get("Accept")
		w.Write([]byte(`{}`))
	}, Options{Header: http.Header{"Accept": {"application/json"}}})
	c.Get(context.Background(), srv.URL)
	if !strings.HasPrefix(ua, "leadtestkit/") || accept != "application/json" {
		t.Errorf("ua = %q accept = %q", ua, accept)
	}
}

func TestRetryTransientThenSuccess(t *testing.T) {
	var n atomic.Int32
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.WriteHeader(503)
		case 2:
			w.WriteHeader(500)
		default:
			w.Write([]byte(`ok`))
		}
	}, Options{})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil || string(resp.Body) != "ok" || n.Load() != 3 {
		t.Fatalf("resp=%v err=%v n=%d", resp, err, n.Load())
	}
}

func TestRetryExhausted(t *testing.T) {
	var n atomic.Int32
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(502)
		w.Write([]byte("bad gateway"))
	}, Options{Attempts: 3})
	_, err := c.Get(context.Background(), srv.URL)
	if StatusCode(err) != 502 || n.Load() != 3 {
		t.Fatalf("err=%v n=%d", err, n.Load())
	}
}

func TestNoRetry(t *testing.T) {
	var n atomic.Int32
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(500)
	}, Options{})
	_, err := c.Do(context.Background(), Request{Method: "POST", URL: srv.URL, NoRetry: true})
	if StatusCode(err) != 500 || n.Load() != 1 {
		t.Fatalf("err=%v n=%d", err, n.Load())
	}
}

func TestNotFoundRetries(t *testing.T) {
	var n atomic.Int32
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte("found"))
	}, Options{})
	if _, err := c.Get(context.Background(), srv.URL); !IsNotFound(err) || n.Load() != 1 {
		t.Fatalf("plain 404: err=%v n=%d", err, n.Load())
	}
	n.Store(0)
	resp, err := c.Do(context.Background(), Request{URL: srv.URL, NotFoundRetries: 2})
	if err != nil || string(resp.Body) != "found" {
		t.Fatalf("err=%v", err)
	}
}

func TestClientErrorNotRetried(t *testing.T) {
	var n atomic.Int32
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(400)
		w.Write([]byte(`{"detail":"lorem"}`))
	}, Options{})
	_, err := c.Get(context.Background(), srv.URL)
	if StatusCode(err) != 400 || n.Load() != 1 || !strings.Contains(err.Error(), "lorem") {
		t.Fatalf("err=%v n=%d", err, n.Load())
	}
}

func TestConnectionErrorRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()
	var sleeps int
	c := New(Options{Attempts: 3, Sleep: func(ctx context.Context, d time.Duration) error {
		sleeps++
		return nil
	}})
	if _, err := c.Get(context.Background(), addr); err == nil || sleeps != 2 {
		t.Fatalf("err=%v sleeps=%d", err, sleeps)
	}
}

func TestRetryAfterHonored(t *testing.T) {
	var n atomic.Int32
	var slept []time.Duration
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			return
		}
		w.Write([]byte("ok"))
	}, Options{BaseDelay: time.Millisecond,
		Sleep: func(ctx context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}})
	if _, err := c.Get(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("slept = %v", slept)
	}
}

func TestRetryAfterTooLong(t *testing.T) {
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(503)
	}, Options{})
	if _, err := c.Get(context.Background(), srv.URL); err == nil ||
		!strings.Contains(err.Error(), "asked to wait") {
		t.Fatalf("err = %v", err)
	}
}

func TestGitHubRateLimit403(t *testing.T) {
	resp := &Response{StatusCode: 403, Header: http.Header{}, Body: []byte("forbidden")}
	if isTransientStatus(resp) {
		t.Error("plain 403 must not be transient")
	}
	resp.Header.Set("X-RateLimit-Remaining", "0")
	if !isTransientStatus(resp) {
		t.Error("rate limited 403 must be transient")
	}
	resp = &Response{StatusCode: 403, Header: http.Header{},
		Body: []byte(`{"message":"You have exceeded a secondary rate limit"}`)}
	if !isTransientStatus(resp) {
		t.Error("secondary rate limit must be transient")
	}
}

func TestRetryAfterDelay(t *testing.T) {
	now := time.Unix(1000, 0)
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset", "1030")
	if d := retryAfterDelay(h, now); d != 31*time.Second {
		t.Errorf("reset delay = %v", d)
	}
	h = http.Header{}
	h.Set("Retry-After", now.Add(10*time.Second).UTC().Format(http.TimeFormat))
	if d := retryAfterDelay(h, now); d != 10*time.Second {
		t.Errorf("date delay = %v", d)
	}
}

func TestHTMLErrorBodyNotIncluded(t *testing.T) {
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(404)
		w.Write([]byte("<html>" + strings.Repeat("x", 5000) + "</html>"))
	}, Options{})
	_, err := c.Get(context.Background(), srv.URL)
	if err == nil || len(err.Error()) > 200 {
		t.Fatalf("err = %v", err)
	}
}

func TestIsBotResponse(t *testing.T) {
	tests := []struct {
		name   string
		status int
		ct     string
		want   bool
	}{
		{"200 html = bot", 200, "text/html; charset=utf-8", true},
		{"200 json = ok", 200, "application/json", false},
		{"200 empty ct = ok", 200, "", false},
		{"403 html = bot", 403, "text/html", true},
		{"503 html = bot", 503, "text/html", true},
		{"500 html = server error", 500, "text/html", false},
		{"502 html = server error", 502, "text/html", false},
		{"504 html = server error", 504, "text/html", false},
		{"404 html = not bot", 404, "text/html", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &Response{StatusCode: tt.status, Header: http.Header{}}
			if tt.ct != "" {
				resp.Header.Set("Content-Type", tt.ct)
			}
			if got := isBotResponse(resp); got != tt.want {
				t.Errorf("isBotResponse(%d, %q) = %v", tt.status, tt.ct, got)
			}
		})
	}
}

func TestBotDetectionTier2CurlWithUA(t *testing.T) {
	if !curlAvailable() {
		t.Skip("curl not installed")
	}
	var n atomic.Int32
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>challenge</html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":1}`))
	}, Options{BotTiers: true})
	resp, err := c.Get(context.Background(), srv.URL+"/test")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if transportMode(c.transport.Load()) != transportCurl {
		t.Errorf("transport = %d, want curl", c.transport.Load())
	}
}

func TestBotDetectionTier3CurlAnon(t *testing.T) {
	if !curlAvailable() {
		t.Skip("curl not installed")
	}
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("User-Agent"), "leadtestkit") {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>challenge</html>"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":1}`))
	}, Options{BotTiers: true})
	if _, err := c.Get(context.Background(), srv.URL+"/test"); err != nil {
		t.Fatal(err)
	}
	if transportMode(c.transport.Load()) != transportCurlAnon {
		t.Errorf("transport = %d, want curl-anon", c.transport.Load())
	}
}

func TestBotDetectionAllBlocked(t *testing.T) {
	if !curlAvailable() {
		t.Skip("curl not installed")
	}
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>challenge</html>"))
	}, Options{BotTiers: true, Attempts: 1})
	if _, err := c.Get(context.Background(), srv.URL); !errors.Is(err, ErrBotProtection) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoBotTiersReturnsHTML(t *testing.T) {
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>page</html>"))
	}, Options{})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil || resp.Via != "go" {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
}

func TestCurlFirst(t *testing.T) {
	if !curlAvailable() {
		t.Skip("curl not installed")
	}
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>archive</html>"))
	}, Options{CurlFirst: true})
	resp, err := c.Get(context.Background(), srv.URL)
	if err != nil || resp.Via != "curl" || string(resp.Body) != "<html>archive</html>" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

func TestCurlFirstChallengeIsError(t *testing.T) {
	if !curlAvailable() {
		t.Skip("curl not installed")
	}
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>Making sure you're not a bot!</html>"))
	}, Options{CurlFirst: true, Attempts: 2})
	if _, err := c.Get(context.Background(), srv.URL); !errors.Is(err, ErrBotProtection) {
		t.Fatalf("err = %v", err)
	}
}

func TestPostBodyViaCurl(t *testing.T) {
	if !curlAvailable() {
		t.Skip("curl not installed")
	}
	var body, ct, auth string
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1024)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		ct = r.Header.Get("Content-Type")
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}, Options{Authorization: "Token lorem"})
	u, _ := url.Parse(srv.URL)
	c.opts.AuthHost = u.Hostname()
	c.transport.Store(int32(transportCurl))
	payload := `{"description":"a \"quoted\" \\ value\nline"}`
	if _, err := c.Do(context.Background(), Request{Method: "POST", URL: srv.URL,
		Body: []byte(payload)}); err != nil {
		t.Fatal(err)
	}
	if body != payload || ct != "application/json" || auth != "Token lorem" {
		t.Errorf("body=%q ct=%q auth=%q", body, ct, auth)
	}
}
