// Copyright 2026 Leadlight Authors
// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from leadlight api/client_test.go (commit 1f9d0e7), modified.

package pw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"leadtestkit/internal/httpx"
)

func testClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	hc := httpx.New(httpx.Options{
		Name:       "patchwork",
		HTTPClient: srv.Client(),
		Logger:     slog.New(slog.DiscardHandler),
		Sleep:      func(ctx context.Context, d time.Duration) error { return nil },
	})
	c, err := New(Options{APIURL: srv.URL + "/api/1.2", Project: "test-project", HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestWebURLAndVersion(t *testing.T) {
	tests := []struct{ api, web, ver string }{
		{"https://pw.example.org/api/1.2", "https://pw.example.org", "1.2"},
		{"https://pw.example.org/api/1.3/", "https://pw.example.org", "1.3"},
		{"https://pw.example.org/api/", "https://pw.example.org", ""},
		{"https://pw.example.org/sub/api/1.2", "https://pw.example.org/sub", "1.2"},
	}
	for _, tt := range tests {
		if got := WebURL(tt.api); got != tt.web {
			t.Errorf("WebURL(%q) = %q", tt.api, got)
		}
		if got := APIVersion(tt.api); got != tt.ver {
			t.Errorf("APIVersion(%q) = %q", tt.api, got)
		}
	}
}

func TestPatchWebURL(t *testing.T) {
	c, err := New(Options{APIURL: "https://pw.example.org/api/1.2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.PatchWebURL(42); got != "https://pw.example.org/patch/42/" {
		t.Errorf("got %q", got)
	}
}

func TestTokenSentToPatchwork(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()
	c, _ := New(Options{APIURL: srv.URL + "/api/1.2", Token: "lorem"})
	if _, err := c.GetSeries(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if auth != "Token lorem" {
		t.Errorf("Authorization = %q", auth)
	}
}

func TestPagination(t *testing.T) {
	var c *Client
	var srv *httptest.Server
	c, srv = testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		switch page {
		case "", "1":
			// Patchwork returns http:// links even over https.
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/1.2/patches/1/checks/?page=2>; rel="next"`,
				srv.URL))
			w.Write([]byte(`[{"id":1},{"id":2}]`))
		case "2":
			w.Write([]byte(`[{"id":3}]`))
		}
	}))
	checks, err := c.GetPatchChecks(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 3 || checks[2].ID != 3 {
		t.Errorf("checks = %+v", checks)
	}
}

func TestParseLinkNext(t *testing.T) {
	tests := []struct{ header, want string }{
		{`<https://pw.example.com/api/1.2/patches/?page=2>; rel="next"`,
			"https://pw.example.com/api/1.2/patches/?page=2"},
		{`<https://pw.example.com/?page=2>; rel="next", <https://pw.example.com/?page=5>; rel="last"`,
			"https://pw.example.com/?page=2"},
		{`<https://pw.example.com/?page=5>; rel="last"`, ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := parseLinkNext(tt.header); got != tt.want {
			t.Errorf("parseLinkNext(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestExtractPageCount(t *testing.T) {
	tests := []struct {
		url  string
		want int
	}{
		{"https://pw.example.com/api/1.3/series/?page=3475&per_page=10", 3475},
		{"https://pw.example.com/api/1.3/patches/", 0},
		{"", 0},
	}
	for _, tt := range tests {
		if got := extractPageCount(tt.url); got != tt.want {
			t.Errorf("extractPageCount(%q) = %d, want %d", tt.url, got, tt.want)
		}
	}
}

func TestFixScheme(t *testing.T) {
	c := &Client{baseURL: "https://patchwork.example.com/api/1.2"}
	if got := c.fixScheme("http://patchwork.example.com/api/1.2/patches/?page=2"); got !=
		"https://patchwork.example.com/api/1.2/patches/?page=2" {
		t.Errorf("got %q", got)
	}
	c.baseURL = "http://patchwork.example.com/api/1.2"
	if got := c.fixScheme("http://x/"); got != "http://x/" {
		t.Errorf("got %q", got)
	}
	if got := c.fixScheme(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestGetSeriesRetries404(t *testing.T) {
	var n atomic.Int32
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/1.2/series/50/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if n.Add(1) == 1 {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(Series{ID: 50, Name: "Lorem series",
			Patches: []PatchSummary{{ID: 1}, {ID: 2}}})
	}))
	s, err := c.GetSeries(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "Lorem series" || len(s.Patches) != 2 {
		t.Errorf("series = %+v", s)
	}
}

func TestFindPatchesByMsgID(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("msgid") != "lorem@example.org" ||
			r.URL.Query().Get("project") != "test-project" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		w.Write([]byte(`[{"id":7}]`))
	}))
	ps, err := c.FindPatchesByMsgID(context.Background(), "<lorem@example.org>")
	if err != nil || len(ps) != 1 || ps[0].ID != 7 {
		t.Fatalf("ps=%v err=%v", ps, err)
	}
}

func TestCreateCheck(t *testing.T) {
	var got map[string]string
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/1.2/patches/9/checks/" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.WriteHeader(201)
		w.Write([]byte(`{}`))
	}))
	err := c.CreateCheck(context.Background(), 9, NewCheck{State: "pending",
		TargetURL: "https://example.org/x", Context: "lorem-robot", Description: "ipsum"})
	if err != nil {
		t.Fatal(err)
	}
	if got["state"] != "pending" || got["context"] != "lorem-robot" ||
		got["target_url"] != "https://example.org/x" || got["description"] != "ipsum" {
		t.Errorf("got %v", got)
	}
}

func TestLatestChecks(t *testing.T) {
	l := LatestChecks([]Check{
		{ID: 3, Context: "a", State: "success"},
		{ID: 1, Context: "a", State: "pending"},
		{ID: 2, Context: "b", State: "fail"},
	})
	if len(l) != 2 || l["a"].State != "success" || l["b"].State != "fail" {
		t.Errorf("latest = %v", l)
	}
}

func eventJSON(id int, category, date string, seriesID int) string {
	return fmt.Sprintf(`{"id":%d,"category":%q,"project":%s,"date":%q,"actor":null,`+
		`"payload":{"series":{"id":%d,"name":"Lorem %d"}}}`,
		id, category, testProjectJSON, date, seriesID, seriesID)
}

// eventServer serves events with IDs from newest down to 1, two per
// page, in descending order.
func eventServer(t *testing.T, newest int, requests *[]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, r.URL.RawQuery)
		q := r.URL.Query()
		if q.Get("order") != "-id" || q.Get("category") != "series-completed" ||
			q.Get("project") != "test-project" || q.Get("since") != "" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		page, _ := strconv.Atoi(q.Get("page"))
		first := newest - (page-1)*2
		if first < 1 {
			w.WriteHeader(404)
			return
		}
		var items []string
		for id := first; id > first-2 && id >= 1; id-- {
			items = append(items, eventJSON(id, "series-completed",
				fmt.Sprintf("2026-03-%02dT10:00:00", id), 100+id))
		}
		w.Write([]byte("[" + strings.Join(items, ",") + "]"))
	})
}

func TestWalkEventsToCursor(t *testing.T) {
	var reqs []string
	c, _ := testClient(t, eventServer(t, 7, &reqs))
	var got []int
	cursor, err := c.WalkEvents(context.Background(),
		WalkOptions{Category: "series-completed", Cursor: 3},
		func(ev Event) error {
			got = append(got, ev.Payload.(*SeriesCompletedPayload).Series.ID)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[104 105 106 107]" || cursor != 7 {
		t.Errorf("got %v cursor %d", got, cursor)
	}
	if len(reqs) != 3 {
		t.Errorf("requests = %d, want 3", len(reqs))
	}
}

func TestWalkEventsHandlerErrorKeepsCursor(t *testing.T) {
	var reqs []string
	c, _ := testClient(t, eventServer(t, 5, &reqs))
	cursor, err := c.WalkEvents(context.Background(),
		WalkOptions{Category: "series-completed", Cursor: 1},
		func(ev Event) error {
			if ev.ID == 4 {
				return fmt.Errorf("lorem failure")
			}
			return nil
		})
	if err == nil || cursor != 3 {
		t.Errorf("cursor = %d err = %v", cursor, err)
	}
}

func TestWalkEventsCutoffWithoutCursor(t *testing.T) {
	var reqs []string
	c, _ := testClient(t, eventServer(t, 9, &reqs))
	var got []int
	cutoff := time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC)
	cursor, err := c.WalkEvents(context.Background(),
		WalkOptions{Category: "series-completed", Cutoff: cutoff},
		func(ev Event) error {
			got = append(got, ev.ID)
			return nil
		})
	if err != nil || fmt.Sprint(got) != "[6 7 8 9]" || cursor != 9 {
		t.Errorf("got %v cursor %d err %v", got, cursor, err)
	}
}

func TestWalkEventsOnlyOldEventsSetsCursor(t *testing.T) {
	var reqs []string
	c, _ := testClient(t, eventServer(t, 4, &reqs))
	cursor, err := c.WalkEvents(context.Background(),
		WalkOptions{Category: "series-completed", Cutoff: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		func(ev Event) error {
			t.Errorf("unexpected event %d", ev.ID)
			return nil
		})
	if err != nil || cursor != 4 {
		t.Errorf("cursor %d err %v", cursor, err)
	}
}

func TestWalkEventsFetchErrorKeepsCursor(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	cursor, err := c.WalkEvents(context.Background(),
		WalkOptions{Category: "series-completed", Cursor: 10},
		func(ev Event) error { return nil })
	if err == nil || cursor != 10 {
		t.Errorf("cursor %d err %v", cursor, err)
	}
}
