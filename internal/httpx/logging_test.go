// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package httpx

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"

	"leadtestkit/internal/logx"
)

func TestRequestLogsIncludeDuration(t *testing.T) {
	for _, mode := range []transportMode{transportGo, transportCurl, transportCurlAnon} {
		for _, tc := range []struct {
			name      string
			status    int
			truncated bool
			cancelled bool
		}{
			{name: "success", status: http.StatusOK},
			{name: "server error", status: http.StatusServiceUnavailable},
			{name: "body read error", status: http.StatusOK, truncated: true},
			{name: "cancelled request", status: http.StatusOK, cancelled: true},
		} {
			t.Run(mode.String()+"/"+tc.name, func(t *testing.T) {
				if mode != transportGo && !curlAvailable() {
					t.Skip("curl is not installed")
				}
				var buf bytes.Buffer
				handler := func(w http.ResponseWriter, r *http.Request) {
					if tc.truncated {
						w.Header().Set("Content-Length", "100")
					}
					w.WriteHeader(tc.status)
					w.Write([]byte("body"))
				}
				opts := Options{Logger: logx.New(&buf, false)}
				c, srv := testClient(t, handler, opts)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.cancelled {
					cancel()
				}
				r := Request{Method: http.MethodGet, URL: srv.URL}
				var err error
				if mode == transportGo {
					_, err = c.viaGo(ctx, r)
				} else {
					_, err = c.viaCurl(ctx, r, mode)
				}
				if (err != nil) != (tc.cancelled || tc.truncated) {
					t.Fatalf("unexpected error: %v", err)
				}
				re := regexp.MustCompile(`(?:in|after) (\S+) using `)
				matches := re.FindAllStringSubmatch(buf.String(), -1)
				if len(matches) != 1 {
					t.Fatalf("expected one timed outcome:\n%s", buf.String())
				}
				d, err := time.ParseDuration(matches[0][1])
				if err != nil || d < 0 {
					t.Fatalf("invalid duration %q: %v", matches[0][1], err)
				}
			})
		}
	}
}
