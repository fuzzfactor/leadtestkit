// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"leadtestkit/internal/archive"
	"leadtestkit/internal/config"
	"leadtestkit/internal/gh"
	"leadtestkit/internal/httpx"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/report"
	"leadtestkit/internal/state"
)

var errTransient = errors.New("HTTP 503: lorem unavailable")

type fakePW struct {
	events   []pw.Event
	series   map[int]*pw.Series
	patches  map[int]*pw.PatchDetail
	covers   map[int]*pw.CoverDetail
	comments map[int][]pw.Comment
	checks   map[int][]pw.Check
	posted   []postedCheck
	fail     map[string]int
	calls    map[string]int
}

type postedCheck struct {
	patch int
	check pw.NewCheck
}

func newFakePW() *fakePW {
	return &fakePW{
		series: map[int]*pw.Series{}, patches: map[int]*pw.PatchDetail{},
		covers: map[int]*pw.CoverDetail{}, comments: map[int][]pw.Comment{},
		checks: map[int][]pw.Check{}, fail: map[string]int{}, calls: map[string]int{},
	}
}

func (f *fakePW) maybeFail(op string) error {
	f.calls[op]++
	if f.fail[op] > 0 {
		f.fail[op]--
		return errTransient
	}
	return nil
}

func (f *fakePW) WalkEvents(ctx context.Context, opts pw.WalkOptions,
	handle func(pw.Event) error) (int, error) {
	if err := f.maybeFail("events"); err != nil {
		return opts.Cursor, err
	}
	cursor := opts.Cursor
	evs := append([]pw.Event(nil), f.events...)
	sort.Slice(evs, func(i, j int) bool { return evs[i].ID < evs[j].ID })
	for _, ev := range evs {
		if ev.Category != opts.Category || ev.ID <= cursor {
			continue
		}
		if opts.Cursor == 0 && !opts.Cutoff.IsZero() &&
			pw.ParseDate(ev.Date).Before(opts.Cutoff) {
			cursor = ev.ID
			continue
		}
		if err := handle(ev); err != nil {
			return cursor, err
		}
		cursor = ev.ID
	}
	return cursor, nil
}

func (f *fakePW) BuildSeriesURL(params pw.SeriesListParams) string { return "series-list" }

func (f *fakePW) GetSeriesPage(ctx context.Context, u string) (*pw.PageResult[pw.Series], error) {
	if err := f.maybeFail("series-list"); err != nil {
		return nil, err
	}
	var items []pw.Series
	for _, s := range f.series {
		items = append(items, *s)
	}
	return &pw.PageResult[pw.Series]{Items: items}, nil
}

func (f *fakePW) GetSeries(ctx context.Context, id int) (*pw.Series, error) {
	if err := f.maybeFail("series"); err != nil {
		return nil, err
	}
	s, ok := f.series[id]
	if !ok {
		return nil, &httpx.StatusError{Code: 404}
	}
	c := *s
	return &c, nil
}

func (f *fakePW) GetPatch(ctx context.Context, id int) (*pw.PatchDetail, error) {
	if err := f.maybeFail("patch"); err != nil {
		return nil, err
	}
	p, ok := f.patches[id]
	if !ok {
		return nil, &httpx.StatusError{Code: 404}
	}
	return p, nil
}

func (f *fakePW) GetCover(ctx context.Context, id int) (*pw.CoverDetail, error) {
	if err := f.maybeFail("cover"); err != nil {
		return nil, err
	}
	return f.covers[id], nil
}

func (f *fakePW) GetPatchComments(ctx context.Context, id int) ([]pw.Comment, error) {
	if err := f.maybeFail("comments"); err != nil {
		return nil, err
	}
	return f.comments[id], nil
}

func (f *fakePW) GetPatchChecks(ctx context.Context, id int) ([]pw.Check, error) {
	if err := f.maybeFail("checks"); err != nil {
		return nil, err
	}
	return f.checks[id], nil
}

func (f *fakePW) CreateCheck(ctx context.Context, id int, chk pw.NewCheck) error {
	if err := f.maybeFail("create-check"); err != nil {
		return err
	}
	f.posted = append(f.posted, postedCheck{id, chk})
	t := chk.TargetURL
	f.checks[id] = append(f.checks[id], pw.Check{ID: len(f.posted), Context: chk.Context,
		State: chk.State, TargetURL: &t})
	return nil
}

func (f *fakePW) FindPatchesByMsgID(ctx context.Context, msgid string) ([]pw.Patch, error) {
	for _, p := range f.patches {
		if p.MsgID == msgid {
			return []pw.Patch{p.Patch}, nil
		}
	}
	return nil, nil
}

func (f *fakePW) PatchWebURL(id int) string {
	return fmt.Sprintf("https://pw.example.org/patch/%d/", id)
}

// addSeries registers a complete series with n patches.
func (f *fakePW) addSeries(id, n int, submitter string, date time.Time) {
	s := &pw.Series{
		ID: id, Name: fmt.Sprintf("lorem: Series %d", id), Version: 1,
		Date: date.Format("2006-01-02T15:04:05"),
		Submitter: pw.Person{Name: "Lorem " + submitter,
			Email: submitter + "@example.org"},
		Total:       n,
		ReceivedAll: true,
	}
	for i := 1; i <= n; i++ {
		pid := id*100 + i
		name := fmt.Sprintf("[lorem-dev,%d/%d] lorem: Patch %d of series %d", i, n, i, id)
		msgid := fmt.Sprintf("<%d@example.org>", pid)
		s.Patches = append(s.Patches, pw.PatchSummary{ID: pid, Name: name, MsgID: msgid})
		d := &pw.PatchDetail{
			Headers: map[string]any{"From": s.Submitter.Name + " <" +
				s.Submitter.Email + ">",
				"Subject": name},
			Content: "Lorem ipsum.\n\nSigned-off-by: x <x@example.org>\n",
			Diff:    "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n",
		}
		d.ID, d.Name, d.MsgID, d.State = pid, name, msgid, "new"
		d.Project = pw.ProjectSummary{LinkName: "lorem"}
		d.Series = []pw.SeriesSummary{{ID: id}}
		f.patches[pid] = d
	}
	f.series[id] = s
	f.events = append(f.events, pw.Event{ID: len(f.events) + 1, Category: "series-completed",
		Date:    date.Format("2006-01-02T15:04:05"),
		Payload: &pw.SeriesCompletedPayload{Series: pw.SeriesSummary{ID: id}}})
}

type fakeGH struct {
	branches         map[string]string
	setBranch        int
	runs             map[string][]gh.WorkflowRun
	dispatched       []gh.WorkflowRun
	jobs             map[int64][]gh.Job
	logs             map[int64]string
	active           []gh.WorkflowRun
	reruns           []int64
	failedOnly       []bool
	refuseFailedOnly bool
	merges           []string
	mergeErr         error
	dispatches       []map[string]string
	refs             map[string]string
	deleted          []string
	files            map[string]string
	fail             map[string]int
	nextRun          int64
}

func newFakeGH() *fakeGH {
	return &fakeGH{branches: map[string]string{}, runs: map[string][]gh.WorkflowRun{},
		jobs: map[int64][]gh.Job{}, logs: map[int64]string{}, refs: map[string]string{},
		files: map[string]string{}, fail: map[string]int{}, nextRun: 1000}
}

func (f *fakeGH) maybeFail(op string) error {
	if f.fail[op] > 0 {
		f.fail[op]--
		return errTransient
	}
	return nil
}

func (f *fakeGH) RefSHA(ctx context.Context, name string) (string, error) {
	if err := f.maybeFail("ref"); err != nil {
		return "", err
	}
	return f.branches[strings.TrimPrefix(name, "heads/")], nil
}

func (f *fakeGH) SetBranch(ctx context.Context, branch, sha string) error {
	if err := f.maybeFail("set-branch"); err != nil {
		return err
	}
	if f.branches[branch] != sha {
		f.setBranch++
	}
	f.branches[branch] = sha
	return nil
}

func (f *fakeGH) DeleteRef(ctx context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeGH) MatchingRefs(ctx context.Context, prefix string) (map[string]string, error) {
	out := map[string]string{}
	for r, sha := range f.refs {
		if strings.HasPrefix(r, "refs/"+prefix) {
			out[r] = sha
		}
	}
	return out, nil
}

func (f *fakeGH) RunsForSHA(ctx context.Context, sha string) ([]gh.WorkflowRun, error) {
	if err := f.maybeFail("runs"); err != nil {
		return nil, err
	}
	return f.runs[sha], nil
}

func (f *fakeGH) DispatchRuns(ctx context.Context, branch string,
	since time.Time) ([]gh.WorkflowRun, error) {
	return f.dispatched, nil
}

func (f *fakeGH) ActiveRuns(ctx context.Context) ([]gh.WorkflowRun, error) { return f.active, nil }

func (f *fakeGH) Jobs(ctx context.Context, id int64) ([]gh.Job, error) {
	if err := f.maybeFail("jobs"); err != nil {
		return nil, err
	}
	return f.jobs[id], nil
}

func (f *fakeGH) JobLog(ctx context.Context, id int64) (string, error) { return f.logs[id], nil }

func (f *fakeGH) Rerun(ctx context.Context, id int64, failedOnly bool) error {
	if err := f.maybeFail("rerun"); err != nil {
		return err
	}
	if failedOnly && f.refuseFailedOnly {
		return &httpx.StatusError{Code: 403}
	}
	f.reruns = append(f.reruns, id)
	f.failedOnly = append(f.failedOnly, failedOnly)
	return nil
}

func (f *fakeGH) Dispatch(ctx context.Context, file, ref string, inputs map[string]string) error {
	if err := f.maybeFail("dispatch"); err != nil {
		return err
	}
	in := map[string]string{"file": file, "ref": ref}
	for k, v := range inputs {
		in[k] = v
	}
	f.dispatches = append(f.dispatches, in)
	return nil
}

func (f *fakeGH) WorkflowFiles(ctx context.Context, ref string) ([]string, error) {
	var out []string
	for name := range f.files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeGH) File(ctx context.Context, path, ref string) ([]byte, error) {
	return []byte(f.files[strings.TrimPrefix(path, ".github/workflows/")]), nil
}

func (f *fakeGH) MergeUpstream(ctx context.Context, branch string) (string, error) {
	f.merges = append(f.merges, branch)
	if f.mergeErr != nil {
		return "", f.mergeErr
	}
	return "merge", nil
}

// startRuns creates push runs for the head of a branch.
func (f *fakeGH) startRuns(branch, sha string, names ...string) []int64 {
	var ids []int64
	for _, n := range names {
		f.nextRun++
		url := fmt.Sprintf("https://github.com/ipsum/lorem/actions/runs/%d", f.nextRun)
		f.runs[sha] = append(f.runs[sha], gh.WorkflowRun{
			ID: f.nextRun, Name: n, Event: "push", HeadBranch: branch, HeadSHA: sha,
			Status: "in_progress", RunAttempt: 1, HTMLURL: url,
		})
		ids = append(ids, f.nextRun)
	}
	return ids
}

func (f *fakeGH) finishRun(id int64, conclusion string) {
	for sha, runs := range f.runs {
		for i := range runs {
			if runs[i].ID == id {
				runs[i].Status = "completed"
				runs[i].Conclusion = conclusion
			}
		}
		f.runs[sha] = runs
	}
}

func (f *fakeGH) restartRun(id int64) {
	for sha, runs := range f.runs {
		for i := range runs {
			if runs[i].ID == id {
				runs[i].Status = "queued"
				runs[i].Conclusion = ""
				runs[i].RunAttempt++
			}
		}
		f.runs[sha] = runs
	}
}

type fakeRepo struct {
	failAt   map[int]int
	pushed   map[string]string
	parents  map[string]string
	applies  int
	applied  int
	resolved []string
	files    map[string]string
	fail     map[string]int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{failAt: map[int]int{}, pushed: map[string]string{},
		files: map[string]string{},
		fail:  map[string]int{}, parents: map[string]string{}}
}

func (f *fakeRepo) Resolve(ctx context.Context, url, branch string) (string, error) {
	if f.fail["resolve"] > 0 {
		f.fail["resolve"]--
		return "", errTransient
	}
	f.resolved = append(f.resolved, url+"#"+branch)
	return "base-" + branch, nil
}

func (f *fakeRepo) ResolveTestRef(ctx context.Context, ref string) (string, error) {
	sha, ok := f.pushed[ref]
	if !ok {
		return "", errors.New("no such ref")
	}
	return sha, nil
}

func (f *fakeRepo) ReadFile(ctx context.Context, rev, path string) ([]byte, error) {
	return []byte(f.files[path]), nil
}

// Apply derives commits from the Message-IDs of the emails.  Like git
// am, applying again creates different commits.
func (f *fakeRepo) Apply(
	ctx context.Context, base string, mboxes [][]byte,
) ([]string, int, string, error) {
	f.applies++
	var shas []string
	parent := base
	for i, m := range mboxes {
		id := ""
		for _, l := range strings.Split(string(m), "\n") {
			if strings.HasPrefix(l, "Message-Id: <") {
				id = strings.TrimPrefix(l, "Message-Id: <")
				id = strings.TrimSuffix(id, "@example.org>")
			}
		}
		var pid int
		fmt.Sscan(id, &pid)
		if fi, ok := f.failAt[pid/100]; ok && fi == i {
			out := "error: patch failed: f:1\nerror: f: patch does not apply"
			return shas, i, out, nil
		}
		sha := "sha-" + id
		if f.applies > 1 {
			sha = fmt.Sprintf("sha-%s.%d", id, f.applies)
		}
		f.parents[sha] = parent
		parent = sha
		shas = append(shas, sha)
		f.applied++
	}
	return shas, -1, "", nil
}

func (f *fakeRepo) PushRefs(ctx context.Context, refs map[string]string) error {
	for ref, sha := range refs {
		f.pushed[ref] = sha
	}
	return nil
}

func (f *fakeRepo) TestRefs(ctx context.Context, refs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range refs {
		if sha, ok := f.pushed[r]; ok {
			out[r] = sha
		}
	}
	return out, nil
}

func (f *fakeRepo) Parent(ctx context.Context, sha string) (string, error) {
	p, ok := f.parents[sha]
	if !ok {
		return "", errors.New("unknown commit")
	}
	return p, nil
}

func (f *fakeRepo) ChangedFiles(ctx context.Context, from, to string) ([]string, error) {
	return nil, nil
}

type fakeArchive struct {
	base     string
	months   map[string][]archive.ArchiveMessage
	messages map[string]*archive.Message
	fetched  []string
	fail     map[string]int

	monthFetches int
	monthFail    int
}

func newFakeArchive(base string) *fakeArchive {
	return &fakeArchive{base: base, months: map[string][]archive.ArchiveMessage{},
		messages: map[string]*archive.Message{}, fail: map[string]int{}}
}

func (f *fakeArchive) FetchMonth(ctx context.Context, y int,
	m time.Month) ([]archive.ArchiveMessage, error) {
	f.monthFetches++
	if f.monthFail > 0 {
		f.monthFail--
		return nil, errTransient
	}
	return f.months[archive.MonthKey(y, m)], nil
}

func (f *fakeArchive) FetchMessage(ctx context.Context, u string) (*archive.Message, error) {
	f.fetched = append(f.fetched, u)
	if f.fail[u] > 0 {
		f.fail[u]--
		return nil, errTransient
	}
	m, ok := f.messages[u]
	if !ok {
		return nil, &httpx.StatusError{Code: 404}
	}
	return m, nil
}

func (f *fakeArchive) MessageURL(month string, n int) string {
	return fmt.Sprintf("%s%s/%d.html", f.base, month, n)
}

// add publishes a message in the archive.
func (f *fakeArchive) add(t time.Time, n int, subject, body, msgid string) string {
	key := archive.MonthKey(t.Year(), t.Month())
	f.months[key] = append(f.months[key], archive.ArchiveMessage{Number: n, Subject: subject})
	u := f.MessageURL(key, n)
	f.messages[u] = &archive.Message{URL: u, Subject: subject, Body: body, MessageID: msgid}
	return u
}

type fakeMail struct {
	sent []*report.Email
	fail int
}

func (f *fakeMail) Send(ctx context.Context, e *report.Email) error {
	if f.fail > 0 {
		f.fail--
		return errTransient
	}
	f.sent = append(f.sent, e)
	return nil
}

func (f *fakeMail) subjects() []string {
	var out []string
	for _, e := range f.sent {
		for _, l := range strings.Split(string(e.Data), "\r\n") {
			if strings.HasPrefix(l, "Subject: ") {
				out = append(out, strings.TrimPrefix(l, "Subject: "))
			}
		}
	}
	return out
}

const baseConfig = `
enabled: true
patchwork:
  api_url: https://pw.example.org/api/1.2
  project: lorem
upstream:
  git_url: https://git.example.org/lorem.git
test_repo:
  repo: ipsum/lorem
report:
  to: lorem-build@lists.example.org
  from: "Robot <robot@example.org>"
archive:
  reports_url: https://lists.example.org/pipermail/lorem-build/
  dev_url: https://lists.example.org/pipermail/lorem-dev/
  ingest: true
smtp:
  host: smtp.example.org
mode:
  push: live
  checks: post
  email: live
rules:
  skip:
    - subject: '\bRFC\b'
      reason: RFC
  base:
    - type: subject
      match: '\b(branch-\d+\.\d+)\b'
      branch: '$1'
  supersede: true
limits:
  max_queued_jobs: 100
  max_inflight_patches: 10
  per_author_min: 2
  jobs_per_patch: 1
  initial_backfill: 1d
  grace_after_runs: 10m
  no_run_timeout: 1h
runner:
  loop_interval: 3m
`

type env struct {
	t       *testing.T
	now     time.Time
	cfg     *config.Config
	pw      *fakePW
	gh      *fakeGH
	repo    *fakeRepo
	reports *fakeArchive
	dev     *fakeArchive
	mail    *fakeMail
	dir     string
	p       *Pipeline
}

func newEnv(t *testing.T, extra string) *env {
	t.Helper()
	cfg, err := config.Parse([]byte(baseConfig + extra))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		t:       t,
		now:     time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
		cfg:     cfg,
		pw:      newFakePW(),
		gh:      newFakeGH(),
		repo:    newFakeRepo(),
		reports: newFakeArchive("https://lists.example.org/pipermail/lorem-build/"),
		dev:     newFakeArchive("https://lists.example.org/pipermail/lorem-dev/"),
		mail:    &fakeMail{},
		dir:     t.TempDir(),
	}
	e.reload()
	return e
}

// reload simulates a new run: the state is read back from disk and a
// fresh pipeline is created.
func (e *env) reload() {
	e.t.Helper()
	if e.p != nil {
		if err := e.p.Store.Save(); err != nil {
			e.t.Fatal(err)
		}
	}
	st, err := state.Load(e.dir)
	if err != nil {
		e.t.Fatal(err)
	}
	e.p = &Pipeline{
		Cfg: e.cfg, Store: st, PW: e.pw, GH: e.gh, Repo: e.repo, Mail: e.mail,
		Reports: e.reports, Dev: e.dev,
		Now: func() time.Time { return e.now },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Checkpoint: func(ctx context.Context, msg string) error {
			return e.p.Store.Save()
		},
	}
}

func (e *env) step() {
	e.p.Step(context.Background(), false)
}

func (e *env) advance(d time.Duration) {
	e.now = e.now.Add(d)
}

func (e *env) series(id int) *state.Series {
	if s := e.p.Store.Find(id); s != nil {
		return s
	}
	recent, _ := e.p.Store.Recent(e.now, 365*24*time.Hour)
	for _, s := range recent {
		if s.ID == id {
			return s
		}
	}
	e.t.Fatalf("series %d not found", id)
	return nil
}

func comments(contents ...string) []pw.Comment {
	var out []pw.Comment
	for i, c := range contents {
		out = append(out, pw.Comment{ID: i + 1, Content: c})
	}
	return out
}
