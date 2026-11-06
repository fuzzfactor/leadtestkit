// Copyright 2026 Leadlight Authors
// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from leadlight api/client.go (commit 1f9d0e7), modified.

package pw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"leadtestkit/internal/httpx"
)

const perPage = "1024"

// ErrInvalidPage is returned for a page number past the last page.
var ErrInvalidPage = errors.New("invalid page")

type Options struct {
	APIURL   string
	Project  string
	Token    string
	MinDelay time.Duration
	Logger   *slog.Logger

	HTTP *httpx.Client
}

type Client struct {
	http    *httpx.Client
	baseURL string
	webURL  string
	project string
}

var apiSuffixRe = regexp.MustCompile(`/api(/\d+\.\d+)?/?$`)

// WebURL derives the web root from an API root like
// https://patchwork.example.org/api/1.2.
func WebURL(apiURL string) string {
	return strings.TrimRight(apiSuffixRe.ReplaceAllString(strings.TrimRight(apiURL, "/"), ""), "/")
}

// APIVersion returns "1.2" for .../api/1.2, or "" if not versioned.
func APIVersion(apiURL string) string {
	m := apiSuffixRe.FindStringSubmatch(strings.TrimRight(apiURL, "/"))
	if len(m) < 2 {
		return ""
	}
	return strings.TrimPrefix(m[1], "/")
}

func New(opts Options) (*Client, error) {
	u, err := url.Parse(opts.APIURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid patchwork API URL %q", opts.APIURL)
	}
	hc := opts.HTTP
	if hc == nil {
		auth := ""
		if opts.Token != "" {
			auth = "Token " + opts.Token
		}
		hc = httpx.New(httpx.Options{
			Name:          "patchwork",
			MinDelay:      opts.MinDelay,
			BotTiers:      true,
			Header:        http.Header{"Accept": {"application/json"}},
			Authorization: auth,
			AuthHost:      u.Hostname(),
			Logger:        opts.Logger,
		})
	}
	base := strings.TrimRight(opts.APIURL, "/")
	return &Client{http: hc, baseURL: base, webURL: WebURL(base), project: opts.Project}, nil
}

func (c *Client) Project() string { return c.project }

// PatchWebURL returns the numeric legacy web URL.  Patchwork redirects
// it to the canonical page, and report parsers expect `patch/<id>`.
func (c *Client) PatchWebURL(id int) string {
	return fmt.Sprintf("%s/patch/%d/", c.webURL, id)
}

func (c *Client) getJSON(ctx context.Context, path string, params url.Values, dest any) error {
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	// Objects referenced by events exist; a 404 from an overloaded
	// server is retried a couple of times before giving up.
	resp, err := c.http.Do(ctx, httpx.Request{URL: u, NotFoundRetries: 2})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(resp.Body, dest); err != nil {
		return fmt.Errorf("decode %s: %w", u, err)
	}
	return nil
}

func getAll[T any](c *Client, ctx context.Context, path string, params url.Values) ([]T, error) {
	var all []T
	u := c.baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	for u != "" {
		resp, err := c.http.Get(ctx, u)
		if err != nil {
			return all, err
		}
		var page []T
		if err := json.Unmarshal(resp.Body, &page); err != nil {
			return all, fmt.Errorf("decode %s: %w", u, err)
		}
		all = append(all, page...)
		u = c.fixScheme(parseLinkNext(resp.Header.Get("Link")))
	}
	return all, nil
}

// Patchwork sometimes returns http:// URLs in Link headers even when
// accessed over https://.
func (c *Client) fixScheme(u string) string {
	if u == "" {
		return u
	}
	if strings.HasPrefix(c.baseURL, "https://") && strings.HasPrefix(u, "http://") {
		return "https://" + u[len("http://"):]
	}
	return u
}

func parseLinkRel(header, rel string) string {
	if header == "" {
		return ""
	}
	target := `rel="` + rel + `"`
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, target) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start >= 0 && end > start {
			return part[start+1 : end]
		}
	}
	return ""
}

func parseLinkNext(header string) string { return parseLinkRel(header, "next") }
func parseLinkLast(header string) string { return parseLinkRel(header, "last") }

func extractPageCount(lastURL string) int {
	if lastURL == "" {
		return 0
	}
	u, err := url.Parse(lastURL)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(u.Query().Get("page"))
	return n
}

type PageResult[T any] struct {
	Items      []T
	NextURL    string
	TotalPages int
}

func getPage[T any](c *Client, ctx context.Context, rawURL string) (*PageResult[T], error) {
	resp, err := c.http.Get(ctx, rawURL)
	if httpx.IsNotFound(err) {
		return nil, ErrInvalidPage
	}
	if err != nil {
		return nil, err
	}
	var items []T
	if err := json.Unmarshal(resp.Body, &items); err != nil {
		return nil, fmt.Errorf("decode %s: %w", rawURL, err)
	}
	link := resp.Header.Get("Link")
	return &PageResult[T]{
		Items:      items,
		NextURL:    c.fixScheme(parseLinkNext(link)),
		TotalPages: extractPageCount(c.fixScheme(parseLinkLast(link))),
	}, nil
}

func (c *Client) GetProject(ctx context.Context, nameOrID string) (*Project, error) {
	var p Project
	if err := c.getJSON(ctx, fmt.Sprintf("/projects/%s/", nameOrID), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

type SeriesListParams struct {
	Project string
	Order   string
	PerPage int
}

func (c *Client) BuildSeriesURL(params SeriesListParams) string {
	v := url.Values{}
	v.Set("per_page", perPage)
	if params.PerPage > 0 {
		v.Set("per_page", strconv.Itoa(params.PerPage))
	}
	if params.Project != "" {
		v.Set("project", params.Project)
	}
	if params.Order != "" {
		v.Set("order", params.Order)
	}
	return c.baseURL + "/series/?" + v.Encode()
}

func (c *Client) GetSeriesPage(ctx context.Context, pageURL string) (*PageResult[Series], error) {
	return getPage[Series](c, ctx, pageURL)
}

type EventListParams struct {
	Project  string
	Order    string
	Category string
}

func (c *Client) BuildEventsURL(params EventListParams) string {
	v := url.Values{}
	v.Set("per_page", perPage)
	if params.Project != "" {
		v.Set("project", params.Project)
	}
	if params.Order != "" {
		v.Set("order", params.Order)
	}
	if params.Category != "" {
		v.Set("category", params.Category)
	}
	return c.baseURL + "/events/?" + v.Encode()
}

func (c *Client) GetEventsPage(ctx context.Context, pageURL string) (*PageResult[Event], error) {
	return getPage[Event](c, ctx, pageURL)
}

func (c *Client) GetPatch(ctx context.Context, id int) (*PatchDetail, error) {
	var pd PatchDetail
	if err := c.getJSON(ctx, fmt.Sprintf("/patches/%d/", id), nil, &pd); err != nil {
		return nil, err
	}
	return &pd, nil
}

// FindPatchesByMsgID looks a patch up by Message-ID, with or without
// angle brackets.
func (c *Client) FindPatchesByMsgID(ctx context.Context, msgid string) ([]Patch, error) {
	v := url.Values{}
	v.Set("msgid", strings.Trim(strings.TrimSpace(msgid), "<>"))
	if c.project != "" {
		v.Set("project", c.project)
	}
	return getAll[Patch](c, ctx, "/patches/", v)
}

func (c *Client) GetPatchComments(ctx context.Context, id int) ([]Comment, error) {
	v := url.Values{}
	v.Set("per_page", perPage)
	return getAll[Comment](c, ctx, fmt.Sprintf("/patches/%d/comments/", id), v)
}

func (c *Client) GetPatchChecks(ctx context.Context, id int) ([]Check, error) {
	v := url.Values{}
	v.Set("per_page", perPage)
	return getAll[Check](c, ctx, fmt.Sprintf("/patches/%d/checks/", id), v)
}

type NewCheck struct {
	State       string `json:"state"`
	TargetURL   string `json:"target_url,omitempty"`
	Context     string `json:"context"`
	Description string `json:"description,omitempty"`
}

func (c *Client) CreateCheck(ctx context.Context, patchID int, chk NewCheck) error {
	data, err := json.Marshal(chk)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("%s/patches/%d/checks/", c.baseURL, patchID)
	_, err = c.http.Do(ctx, httpx.Request{Method: http.MethodPost, URL: u, Body: data})
	return err
}

func (c *Client) GetSeries(ctx context.Context, id int) (*Series, error) {
	var s Series
	if err := c.getJSON(ctx, fmt.Sprintf("/series/%d/", id), nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (c *Client) GetCover(ctx context.Context, id int) (*CoverDetail, error) {
	var cd CoverDetail
	if err := c.getJSON(ctx, fmt.Sprintf("/covers/%d/", id), nil, &cd); err != nil {
		return nil, err
	}
	return &cd, nil
}

// LatestChecks keeps only the newest check per context; Patchwork
// creates a new record on every state change.
func LatestChecks(checks []Check) map[string]Check {
	latest := map[string]Check{}
	for _, ch := range checks {
		if cur, ok := latest[ch.Context]; !ok || ch.ID > cur.ID {
			latest[ch.Context] = ch
		}
	}
	return latest
}
