// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package gh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"leadtestkit/internal/httpx"
)

func testClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := httpx.New(httpx.Options{
		Name:          "github",
		HTTPClient:    srv.Client(),
		Authorization: "Bearer lorem-token",
		AuthHost:      "127.0.0.1",
		Logger:        slog.New(slog.DiscardHandler),
		Sleep:         func(ctx context.Context, d time.Duration) error { return nil },
	})
	c, err := New(Options{APIURL: srv.URL, Repo: "ipsum/lorem", HTTP: hc})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func TestRefSHA(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/ipsum/lorem/git/ref/leadtest/series_1":
			w.Write([]byte(`{"ref":"refs/leadtest/series_1","object":{"sha":"abc"}}`))
		default:
			w.WriteHeader(404)
		}
	})
	sha, err := c.RefSHA(context.Background(), "leadtest/series_1")
	if sha != "abc" || err != nil {
		t.Errorf("sha=%q err=%v", sha, err)
	}
	if sha, err := c.RefSHA(context.Background(), "heads/none"); sha != "" || err != nil {
		t.Errorf("sha=%q err=%v", sha, err)
	}
}

func TestCheckRunsAndAnnotations(t *testing.T) {
	var srv *httptest.Server
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/ipsum/lorem/check-suites/7/check-runs":
			if r.URL.Query().Get("filter") != "latest" {
				t.Error("only the latest check runs must be listed")
			}
			w.Write([]byte(`{"total_count":2,"check_runs":[` +
				`{"id":11,"name":"checkpatch","output":{"annotations_count":2}},` +
				`{"id":12,"name":"lint","output":{"annotations_count":0}}]}`))
		case r.URL.Path == "/repos/ipsum/lorem/check-runs/11/annotations" &&
			r.URL.Query().Get("page") == "":
			next := srv.URL + "/repos/ipsum/lorem/check-runs/11/annotations?page=2"
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
			w.Write([]byte(`[{"annotation_level":"warning","title":"leadtest",` +
				`"message":"WARNING: Line is 80 characters long"}]`))
		case r.URL.Path == "/repos/ipsum/lorem/check-runs/11/annotations":
			w.Write([]byte(`[{"annotation_level":"failure","title":"",` +
				`"message":"Process completed with exit code 1."}]`))
		default:
			w.WriteHeader(404)
		}
	})
	runs, err := c.CheckRuns(context.Background(), 7)
	if err != nil || len(runs) != 2 || runs[0].ID != 11 ||
		runs[0].Output.AnnotationsCount != 2 {
		t.Fatalf("check runs = %+v, %v", runs, err)
	}
	notes, err := c.Annotations(context.Background(), 11)
	if err != nil || len(notes) != 2 || notes[0].Level != "warning" ||
		notes[0].Title != "leadtest" || notes[1].Level != "failure" {
		t.Fatalf("annotations = %+v, %v", notes, err)
	}
}

func TestSetBranch(t *testing.T) {
	refs := map[string]string{"refs/heads/series_2": "old"}
	var calls []string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Method == "GET" &&
			strings.HasPrefix(r.URL.Path, "/repos/ipsum/lorem/git/ref/"):
			ref := strings.TrimPrefix(r.URL.Path, "/repos/ipsum/lorem/git/ref/")
			name := "refs/" + ref
			sha, ok := refs[name]
			if !ok {
				w.WriteHeader(404)
				return
			}
			fmt.Fprintf(w, `{"object":{"sha":%q}}`, sha)
		case r.Method == "POST" && r.URL.Path == "/repos/ipsum/lorem/git/refs":
			refs[body["ref"].(string)] = body["sha"].(string)
			w.WriteHeader(201)
			w.Write([]byte(`{}`))
		case r.Method == "PATCH":
			if body["force"] != true {
				t.Error("update must be forced")
			}
			refs["refs/heads/"+strings.TrimPrefix(r.URL.Path,
				"/repos/ipsum/lorem/git/refs/heads/")] =
				body["sha"].(string)
			w.Write([]byte(`{}`))
		}
	})
	ctx := context.Background()
	if err := c.SetBranch(ctx, "series_1", "aaa"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetBranch(ctx, "series_2", "bbb"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetBranch(ctx, "series_2", "bbb"); err != nil {
		t.Fatal(err)
	}
	if refs["refs/heads/series_1"] != "aaa" || refs["refs/heads/series_2"] != "bbb" {
		t.Errorf("refs = %v", refs)
	}
	if n := strings.Count(strings.Join(calls, ","), "PATCH"); n != 1 {
		t.Errorf("calls = %v", calls)
	}
}

func TestRunsForSHAPagination(t *testing.T) {
	var srvURL string
	c, srv := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head_sha") != "abc" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		if r.URL.Query().Get("page") == "" {
			next := srvURL + "/repos/ipsum/lorem/actions/runs?head_sha=abc&page=2"
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
			w.Write([]byte(`{"total_count":2,` +
				`"workflow_runs":[{"id":1,"name":"Build"}]}`))
			return
		}
		w.Write([]byte(`{"total_count":2,` +
			`"workflow_runs":[{"id":2,"name":"Test","run_attempt":2}]}`))
	})
	srvURL = srv.URL
	runs, err := c.RunsForSHA(context.Background(), "abc")
	if err != nil || len(runs) != 2 || runs[1].RunAttempt != 2 {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
}

func TestDispatchNotRetried(t *testing.T) {
	var n atomic.Int32
	var body map[string]any
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.URL.Path != "/repos/ipsum/lorem/actions/workflows/checkpatch.yml/dispatches" {
			t.Errorf("path = %q", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(500)
	})
	err := c.Dispatch(context.Background(), "checkpatch.yml", "leadtestkit",
		map[string]string{"sha": "abc"})
	if err == nil || n.Load() != 1 {
		t.Fatalf("err=%v n=%d", err, n.Load())
	}
	if body["ref"] != "leadtestkit" || body["inputs"].(map[string]any)["sha"] != "abc" {
		t.Errorf("body = %v", body)
	}
}

func TestJobLogRedirectDropsAuthorization(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
		r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("Authorization leaked to storage host")
		}
		w.Write([]byte("2026-03-10T10:00:00.0Z lorem log\n"))
	}))
	defer storage.Close()
	storageURL := strings.Replace(storage.URL, "127.0.0.1", "localhost", 1)
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer lorem-token" {
			t.Error("missing Authorization on API host")
		}
		http.Redirect(w, r, storageURL+"/log", http.StatusFound)
	})
	log, err := c.JobLog(context.Background(), 5)
	if err != nil || !strings.Contains(log, "lorem log") {
		t.Fatalf("log=%q err=%v", log, err)
	}
}

func TestWorkflowFilesAndFile(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "leadtestkit" {
			t.Errorf("ref = %q", r.URL.Query().Get("ref"))
		}
		switch r.URL.Path {
		case "/repos/ipsum/lorem/contents/.github/workflows":
			w.Write([]byte(`[{"name":"a.yml","type":"file"},
				{"name":"README","type":"file"},
				{"name":"b.yaml","type":"file"},
				{"name":"d","type":"dir"}]`))
		case "/repos/ipsum/lorem/contents/.github/workflows/a.yml":
			enc := base64.StdEncoding.EncodeToString([]byte("on: workflow_dispatch\n"))
			fmt.Fprintf(w, `{"content":%q,"encoding":"base64"}`, enc[:4]+"\n"+enc[4:])
		}
	})
	files, err := c.WorkflowFiles(context.Background(), "leadtestkit")
	if err != nil || strings.Join(files, ",") != "a.yml,b.yaml" {
		t.Fatalf("files=%v err=%v", files, err)
	}
	data, err := c.File(context.Background(), ".github/workflows/a.yml", "leadtestkit")
	if err != nil || string(data) != "on: workflow_dispatch\n" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

func TestJobs(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"jobs":[{"id":1,"name":"build","conclusion":"failure"}]}`)
	})
	jobs, err := c.Jobs(context.Background(), 1)
	if err != nil || len(jobs) != 1 || jobs[0].Name != "build" {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
}

func TestMergeUpstream(t *testing.T) {
	var n atomic.Int32
	var body map[string]string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.Method != "POST" || r.URL.Path != "/repos/ipsum/lorem/merge-upstream" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&body)
		if n.Load() == 1 {
			w.Write([]byte(`{"message":"ok","merge_type":"merge",` +
				`"base_branch":"upstream:main"}`))
			return
		}
		w.WriteHeader(409)
		w.Write([]byte(`{"message":"conflict"}`))
	})
	kind, err := c.MergeUpstream(context.Background(), "main")
	if err != nil || kind != "merge" || body["branch"] != "main" {
		t.Fatalf("kind=%q err=%v body=%v", kind, err, body)
	}
	if _, err := c.MergeUpstream(context.Background(), "main"); httpx.StatusCode(err) != 409 ||
		n.Load() != 2 {
		t.Fatalf("err=%v n=%d", err, n.Load())
	}
}
