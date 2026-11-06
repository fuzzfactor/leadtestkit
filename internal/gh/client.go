// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package gh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"leadtestkit/internal/httpx"
)

type Options struct {
	APIURL string
	// Repo is "owner/name".
	Repo   string
	Token  string
	Logger *slog.Logger
	HTTP   *httpx.Client
}

type Client struct {
	http *httpx.Client
	api  string
	repo string

	mu     sync.Mutex
	expiry string
}

func New(opts Options) (*Client, error) {
	u, err := url.Parse(opts.APIURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid GitHub API URL %q", opts.APIURL)
	}
	hc := opts.HTTP
	if hc == nil {
		auth := ""
		if opts.Token != "" {
			auth = "Bearer " + opts.Token
		}
		hc = httpx.New(httpx.Options{
			Name: "github",
			Header: http.Header{
				"Accept":               {"application/vnd.github+json"},
				"X-Github-Api-Version": {"2022-11-28"},
			},
			Authorization: auth,
			AuthHost:      u.Hostname(),
			Logger:        opts.Logger,
		})
	}
	return &Client{http: hc, api: strings.TrimRight(opts.APIURL, "/"), repo: opts.Repo}, nil
}

func (c *Client) Repo() string { return c.repo }

// TokenExpiry is the expiration time reported by GitHub for the token,
// or "" if unknown or not expiring.
func (c *Client) TokenExpiry() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.expiry
}

func (c *Client) repoURL(path string) string {
	return c.api + "/repos/" + c.repo + path
}

func (c *Client) do(ctx context.Context, r httpx.Request) (*httpx.Response, error) {
	resp, err := c.http.Do(ctx, r)
	if resp != nil {
		if exp := resp.Header.Get("Github-Authentication-Token-Expiration"); exp != "" {
			c.mu.Lock()
			c.expiry = exp
			c.mu.Unlock()
		}
	}
	return resp, err
}

func (c *Client) getJSON(ctx context.Context, u string, dest any) (*httpx.Response, error) {
	resp, err := c.do(ctx, httpx.Request{URL: u})
	if err != nil {
		return resp, err
	}
	if err := json.Unmarshal(resp.Body, dest); err != nil {
		return resp, fmt.Errorf("decode %s: %w", u, err)
	}
	return resp, nil
}

func (c *Client) sendJSON(ctx context.Context, method, u string, body any, noRetry bool) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, httpx.Request{Method: method, URL: u, Body: data, NoRetry: noRetry})
	return err
}

func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start, end := strings.Index(part, "<"), strings.Index(part, ">")
		if start >= 0 && end > start {
			return part[start+1 : end]
		}
	}
	return ""
}

type ref struct {
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

// RefSHA returns the commit a ref points to, or "" if it does not
// exist.  The ref is given without the "refs/" prefix.
func (c *Client) RefSHA(ctx context.Context, name string) (string, error) {
	var r ref
	_, err := c.getJSON(ctx, c.repoURL("/git/ref/"+name), &r)
	if httpx.IsNotFound(err) {
		return "", nil
	}
	return r.Object.SHA, err
}

// SetBranch points a branch at a commit that already exists in the
// repository, creating it if needed.  Ref updates through the API
// trigger push workflows like a git push.
func (c *Client) SetBranch(ctx context.Context, branch, sha string) error {
	cur, err := c.RefSHA(ctx, "heads/"+branch)
	if err != nil {
		return err
	}
	if cur == sha {
		return nil
	}
	if cur == "" {
		return c.sendJSON(ctx, http.MethodPost, c.repoURL("/git/refs"),
			map[string]string{"ref": "refs/heads/" + branch, "sha": sha}, false)
	}
	return c.sendJSON(ctx, http.MethodPatch, c.repoURL("/git/refs/heads/"+branch),
		map[string]any{"sha": sha, "force": true}, false)
}

func (c *Client) DeleteRef(ctx context.Context, name string) error {
	_, err := c.do(ctx, httpx.Request{Method: http.MethodDelete, URL: c.repoURL("/git/refs/" + name)})
	if httpx.IsNotFound(err) || httpx.StatusCode(err) == http.StatusUnprocessableEntity {
		return nil
	}
	return err
}

func (c *Client) MatchingRefs(ctx context.Context, prefix string) (map[string]string, error) {
	out := map[string]string{}
	u := c.repoURL("/git/matching-refs/" + prefix + "?per_page=100")
	for u != "" {
		var refs []struct {
			Ref    string `json:"ref"`
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		resp, err := c.getJSON(ctx, u, &refs)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			out[r.Ref] = r.Object.SHA
		}
		u = nextLink(resp.Header.Get("Link"))
	}
	return out, nil
}

type WorkflowRun struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	DisplayTitle string    `json:"display_title"`
	Path         string    `json:"path"`
	HeadBranch   string    `json:"head_branch"`
	HeadSHA      string    `json:"head_sha"`
	Event        string    `json:"event"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	HTMLURL      string    `json:"html_url"`
	RunAttempt   int       `json:"run_attempt"`
	RunNumber    int       `json:"run_number"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (c *Client) listRuns(ctx context.Context, params url.Values, maxPages int) ([]WorkflowRun, error) {
	params.Set("per_page", "100")
	u := c.repoURL("/actions/runs?" + params.Encode())
	var all []WorkflowRun
	for page := 0; u != "" && (maxPages == 0 || page < maxPages); page++ {
		var res struct {
			TotalCount   int           `json:"total_count"`
			WorkflowRuns []WorkflowRun `json:"workflow_runs"`
		}
		resp, err := c.getJSON(ctx, u, &res)
		if err != nil {
			return nil, err
		}
		all = append(all, res.WorkflowRuns...)
		u = nextLink(resp.Header.Get("Link"))
	}
	return all, nil
}

func (c *Client) RunsForSHA(ctx context.Context, sha string) ([]WorkflowRun, error) {
	return c.listRuns(ctx, url.Values{"head_sha": {sha}}, 0)
}

func (c *Client) DispatchRuns(ctx context.Context, branch string, since time.Time) ([]WorkflowRun, error) {
	return c.listRuns(ctx, url.Values{
		"event":   {"workflow_dispatch"},
		"branch":  {branch},
		"created": {">=" + since.UTC().Format("2006-01-02T15:04:05Z")},
	}, 10)
}

func (c *Client) ActiveRuns(ctx context.Context) ([]WorkflowRun, error) {
	var all []WorkflowRun
	for _, st := range []string{"queued", "in_progress", "waiting", "pending"} {
		runs, err := c.listRuns(ctx, url.Values{"status": {st}}, 5)
		if err != nil {
			return nil, err
		}
		all = append(all, runs...)
	}
	return all, nil
}

type Step struct {
	Name       string `json:"name"`
	Number     int    `json:"number"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type Job struct {
	ID         int64  `json:"id"`
	RunID      int64  `json:"run_id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	Steps      []Step `json:"steps"`
}

func (c *Client) Jobs(ctx context.Context, runID int64) ([]Job, error) {
	u := c.repoURL(fmt.Sprintf("/actions/runs/%d/jobs?filter=latest&per_page=100", runID))
	var all []Job
	for u != "" {
		var res struct {
			Jobs []Job `json:"jobs"`
		}
		resp, err := c.getJSON(ctx, u, &res)
		if err != nil {
			return nil, err
		}
		all = append(all, res.Jobs...)
		u = nextLink(resp.Header.Get("Link"))
	}
	return all, nil
}

// JobLog downloads the plain text log of a job.  GitHub redirects to a
// storage host; the Go client drops Authorization on that redirect.
func (c *Client) JobLog(ctx context.Context, jobID int64) (string, error) {
	resp, err := c.do(ctx, httpx.Request{URL: c.repoURL(fmt.Sprintf("/actions/jobs/%d/logs", jobID))})
	if err != nil {
		return "", err
	}
	return string(resp.Body), nil
}

func (c *Client) Rerun(ctx context.Context, runID int64, failedOnly bool) error {
	path := fmt.Sprintf("/actions/runs/%d/rerun", runID)
	if failedOnly {
		path = fmt.Sprintf("/actions/runs/%d/rerun-failed-jobs", runID)
	}
	return c.sendJSON(ctx, http.MethodPost, c.repoURL(path), map[string]any{}, true)
}

// Dispatch starts a workflow_dispatch workflow.  It is not retried: a
// duplicate would start a second run, and callers find runs by name
// before dispatching again.
func (c *Client) Dispatch(ctx context.Context, file, ref string, inputs map[string]string) error {
	return c.sendJSON(ctx, http.MethodPost,
		c.repoURL("/actions/workflows/"+url.PathEscape(file)+"/dispatches"),
		map[string]any{"ref": ref, "inputs": inputs}, true)
}

type contentEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func (c *Client) WorkflowFiles(ctx context.Context, ref string) ([]string, error) {
	var entries []contentEntry
	_, err := c.getJSON(ctx, c.repoURL("/contents/.github/workflows?ref="+url.QueryEscape(ref)), &entries)
	if httpx.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.Type == "file" && (strings.HasSuffix(e.Name, ".yml") || strings.HasSuffix(e.Name, ".yaml")) {
			files = append(files, e.Name)
		}
	}
	return files, nil
}

func (c *Client) File(ctx context.Context, path, ref string) ([]byte, error) {
	var e contentEntry
	if _, err := c.getJSON(ctx, c.repoURL("/contents/"+path+"?ref="+url.QueryEscape(ref)), &e); err != nil {
		return nil, err
	}
	if e.Encoding != "base64" {
		return nil, fmt.Errorf("%s: unexpected encoding %q", path, e.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(e.Content, "\n", ""))
}
