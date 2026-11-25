// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"context"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

// tryRecorder records the tries a client reports.
type tryRecorder struct {
	mu    sync.Mutex
	tries []bool
}

func (r *tryRecorder) onTry(failed bool) {
	r.mu.Lock()
	r.tries = append(r.tries, failed)
	r.mu.Unlock()
}

func (r *tryRecorder) check(t *testing.T, want ...bool) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(want) == 0 && len(r.tries) == 0 {
		return
	}
	if !reflect.DeepEqual(r.tries, want) {
		t.Fatalf("tries = %v, want %v", r.tries, want)
	}
}

// statuses answers with the given status codes in turn, then 200.
func statuses(codes ...int) http.HandlerFunc {
	var n atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if i := int(n.Add(1)) - 1; i < len(codes) {
			w.WriteHeader(codes[i])
			return
		}
		w.Write([]byte(`{}`))
	}
}

func TestOnTryTransientThenSuccess(t *testing.T) {
	var rec tryRecorder
	c, srv := testClient(t, statuses(502, 502), Options{Attempts: 3, OnTry: rec.onTry})
	if _, err := c.Get(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	rec.check(t, true, true, false)
}

func TestOnTryClientErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codes []int
		retry int
		want  []bool
	}{
		{"404", []int{404}, 0, []bool{false}},
		{"400", []int{400}, 0, []bool{false}},
		{"404 retried", []int{404}, 1, []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rec tryRecorder
			c, srv := testClient(t, statuses(tc.codes...), Options{OnTry: rec.onTry})
			c.Do(context.Background(), Request{URL: srv.URL, NotFoundRetries: tc.retry})
			rec.check(t, tc.want...)
		})
	}
}

func TestOnTryNetworkError(t *testing.T) {
	var rec tryRecorder
	c, srv := testClient(t, statuses(), Options{Attempts: 2, OnTry: rec.onTry})
	srv.Close()
	if _, err := c.Get(context.Background(), srv.URL); err == nil {
		t.Fatal("no error from a closed server")
	}
	rec.check(t, true, true)
}

func TestOnTryCanceled(t *testing.T) {
	var rec tryRecorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		w.WriteHeader(502)
	}, Options{OnTry: rec.onTry})
	c.Get(ctx, srv.URL)
	rec.check(t)
}

func TestOnTryBotProtection(t *testing.T) {
	if !curlAvailable() {
		t.Skip("curl not installed")
	}
	var rec tryRecorder
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><div id="anubis_challenge"></div></html>`))
	}, Options{CurlFirst: true, Attempts: 2, OnTry: rec.onTry})
	c.Get(context.Background(), srv.URL)
	rec.check(t)
}

func TestOnTryRateLimited(t *testing.T) {
	var rec tryRecorder
	var n atomic.Int32
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(403)
			return
		}
		w.Write([]byte(`{}`))
	}, Options{OnTry: rec.onTry})
	if _, err := c.Get(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	rec.check(t, true, false)
}
