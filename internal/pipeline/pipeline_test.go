// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"leadtestkit/internal/archive"
	"leadtestkit/internal/config"
	"leadtestkit/internal/gh"
	"leadtestkit/internal/httpx"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/state"
)

func TestFullFlow(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(7, 2, "lorem", e.now.Add(-time.Hour))

	e.step()
	s := e.series(7)
	if s.Status != state.SeriesTesting || s.Branch != "series_7" || s.Base.Branch != "main" {
		t.Fatalf("series = %+v", s)
	}
	if e.repo.pushed["refs/leadtest/series_7"] != "sha-702" {
		t.Errorf("staging ref = %v", e.repo.pushed)
	}
	// Both patches are pushed in order, the branch ends at the last one.
	if e.gh.branches["series_7"] != "sha-702" || s.Patches[0].Status != state.PatchPushed {
		t.Fatalf("branches = %v patches = %+v", e.gh.branches, s.Patches[0])
	}
	// Apply results are reported for each patch.
	subjects := e.mail.subjects()
	if len(subjects) != 2 || !strings.HasPrefix(subjects[0],
		"|success| pw701 [lorem-dev,1/2]") {
		t.Fatalf("subjects = %q", subjects)
	}

	e.reload()
	e.gh.startRuns("series_7", "sha-701", "Build", "Test")
	r2 := e.gh.startRuns("series_7", "sha-702", "Build", "Test")
	e.advance(3 * time.Minute)
	e.step()
	s = e.series(7)
	if len(e.pw.posted) != 4 || e.pw.posted[0].check.State != "pending" ||
		e.pw.posted[0].check.TargetURL != "https://github.com/ipsum/lorem/tree/series_7" ||
		e.pw.posted[0].check.Context != "github-robot-_Build" {
		t.Fatalf("posted = %+v", e.pw.posted)
	}
	if n := len(e.mail.subjects()); n != 6 {
		t.Fatalf("emails = %d", n)
	}

	for _, runs := range e.gh.runs {
		for _, r := range runs {
			e.gh.finishRun(r.ID, "success")
		}
	}
	e.gh.finishRun(r2[1], "failure")
	e.gh.jobs[r2[1]] = []gh.Job{{ID: 55, Name: "lorem-job", Conclusion: "failure",
		Steps: []gh.Step{{Name: "make check", Conclusion: "failure"}}}}
	e.gh.logs[55] = "2026-03-10T12:00:00.1234567Z line one\n" +
		"2026-03-10T12:00:01.0Z ipsum error\n"
	e.advance(3 * time.Minute)
	e.step()
	subjects = e.mail.subjects()
	if len(subjects) != 10 {
		t.Fatalf("subjects = %q", subjects)
	}
	last := string(e.mail.sent[len(e.mail.sent)-1].Data)
	if !strings.Contains(last, "|fail| pw702") ||
		!strings.Contains(last, "Cc: \"Lorem lorem\"") ||
		!strings.Contains(last, "lorem-job: failed at step \"make check\"") ||
		!strings.Contains(last, "\r\nipsum error\r\n") {
		t.Errorf("failure report:\n%s", last)
	}
	if e.series(7).Status != state.SeriesTesting {
		t.Fatal("series finished before the grace period")
	}

	e.advance(11 * time.Minute)
	e.step()
	s = e.series(7)
	if s.Status != state.SeriesDone || e.p.Store.Find(7) != nil {
		t.Fatalf("series = %+v", s)
	}
	e.reload()
	if h, _ := e.p.Store.History("2026-03"); len(h) != 1 || h[0].ID != 7 {
		t.Errorf("history = %+v", h)
	}
	if e.p.Store.Meta.Stats.JobsPerPatch == 0 {
		t.Error("jobs per patch not recorded")
	}
}

func TestApplyFailure(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(8, 3, "lorem", e.now)
	e.repo.failAt[8] = 1
	e.step()
	s := e.series(8)
	if s.Patches[0].Status != state.PatchPushed ||
		s.Patches[1].Status != state.PatchApplyFailed ||
		s.Patches[2].Status != state.PatchNotApplied {
		t.Fatalf("patches = %+v %+v %+v", s.Patches[0], s.Patches[1], s.Patches[2])
	}
	if e.gh.branches["series_8"] != "sha-801" {
		t.Errorf("branch = %q", e.gh.branches["series_8"])
	}
	var failMail string
	for _, m := range e.mail.sent {
		if strings.Contains(string(m.Data), "|fail| pw802") {
			failMail = string(m.Data)
		}
	}
	if !strings.Contains(failMail, "Test-Label: apply-robot") ||
		!strings.Contains(failMail, "patch does not apply") ||
		!strings.Contains(failMail, "_apply robot: failed_") {
		t.Errorf("apply failure mail:\n%s", failMail)
	}
}

func TestTransientErrorsAreRetried(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(9, 1, "lorem", e.now)
	e.pw.fail["series"] = 2
	e.step()
	s := e.series(9)
	if s.Status != state.SeriesNew || s.Failure == nil || s.Failure.Attempts != 1 {
		t.Fatalf("series = %+v failure = %+v", s, s.Failure)
	}
	// Not retried before the backoff expires.
	e.advance(time.Minute)
	e.step()
	if s.Failure.Attempts != 1 {
		t.Fatalf("retried too early: %+v", s.Failure)
	}
	e.reload()
	e.advance(5 * time.Minute)
	e.step()
	if e.series(9).Failure.Attempts != 2 {
		t.Fatalf("failure = %+v", e.series(9).Failure)
	}
	e.advance(10 * time.Minute)
	e.step()
	if s := e.series(9); s.Status != state.SeriesTesting || s.Failure != nil {
		t.Fatalf("series = %+v", s)
	}
}

func TestGiveUpAfterDeadline(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(10, 1, "lorem", e.now)
	e.pw.fail["series"] = 1000
	for i := 0; i < 40; i++ {
		e.step()
		e.advance(2 * time.Hour)
	}
	s := e.series(10)
	if s.Status != state.SeriesError || !strings.Contains(s.Reason, "fetch series") {
		t.Fatalf("series = %+v", s)
	}
	if len(e.p.Stats.Errors) == 0 {
		t.Error("giving up must be reported as an error")
	}
}

func TestSkipRulesAndBaseSelection(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(11, 1, "lorem", e.now)
	e.pw.series[11].Patches[0].Name = "[RFC,lorem-dev] lorem: Idea"
	e.pw.addSeries(12, 1, "ipsum", e.now)
	e.pw.patches[1201].Name = "[lorem-dev,branch-3.4] ipsum: Fix"
	e.step()
	if s := e.series(11); s.Status != state.SeriesSkipped || s.Reason != "RFC" {
		t.Errorf("series 11 = %+v", s)
	}
	pt := e.series(11).Patches[0]
	if pt.Status != state.PatchSkipped {
		t.Errorf("patch of a skipped series is %s", pt.Status)
	}
	decision(t, pt.Decisions, state.PatchSkipped, "")
	if s := e.series(12); s.Base == nil || s.Base.Branch != "branch-3.4" ||
		s.Base.Why != "subject: branch-3.4" {
		t.Errorf("series 12 base = %+v", s.Base)
	}
}

func TestSupersede(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Push = "off"
	e.pw.addSeries(20, 1, "lorem", e.now)
	e.step()
	e.pw.addSeries(21, 1, "lorem", e.now)
	e.pw.series[21].Version = 2
	e.pw.series[21].Name = "[v2] lorem: Series 20"
	e.step()
	if s := e.series(20); s.Status != state.SeriesSkipped ||
		!strings.Contains(s.Reason, "superseded") {
		t.Errorf("series 20 = %+v", s)
	}
	if s := e.series(21); s.Status != state.SeriesQueued {
		t.Errorf("series 21 = %+v", s)
	}
}

func TestFairnessAcrossAuthors(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Limits.MaxInflightPatches = 4
	e.pw.addSeries(30, 10, "lorem", e.now.Add(-time.Hour))
	e.pw.addSeries(31, 1, "ipsum", e.now)
	e.step()
	pushed := map[int]int{}
	for _, id := range []int{30, 31} {
		for _, pt := range e.series(id).Patches {
			if pt.Status == state.PatchPushed {
				pushed[id]++
			}
		}
	}
	if pushed[30] != 2 || pushed[31] != 1 {
		t.Errorf("pushed = %v", pushed)
	}
}

func TestQueuedJobsBlockPushing(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Limits.MaxQueuedJobs = 3
	e.gh.active = []gh.WorkflowRun{{ID: 1, Status: "in_progress"}}
	e.gh.jobs[1] = []gh.Job{{Status: "queued"}, {Status: "queued"}, {Status: "queued"}}
	e.pw.addSeries(40, 1, "lorem", e.now)
	e.step()
	if s := e.series(40); s.Status != state.SeriesQueued {
		t.Errorf("series = %+v", s)
	}
}

func TestNoRunsTimeout(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Report.ApplySuccess = false
	e.pw.addSeries(50, 1, "lorem", e.now)
	e.step()
	e.advance(61 * time.Minute)
	e.step()
	pt := e.series(50).Patches[0]
	if len(pt.Runs) != 1 || pt.Runs[0].Kind != state.RunMissing ||
		pt.Runs[0].Final.Email != state.StepSent {
		t.Fatalf("runs = %+v", pt.Runs)
	}
	subjects := e.mail.subjects()
	if len(subjects) != 1 || !strings.HasPrefix(subjects[0], "|warning| pw5001") {
		t.Errorf("subjects = %q", subjects)
	}
}

func TestCancelledRunIsRerunOnce(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(60, 1, "lorem", e.now)
	e.step()
	ids := e.gh.startRuns("series_60", "sha-6001", "Build")
	e.gh.finishRun(ids[0], "cancelled")
	e.step()
	if len(e.gh.reruns) != 1 || !e.gh.failedOnly[0] {
		t.Fatalf("reruns = %v failed only = %v", e.gh.reruns, e.gh.failedOnly)
	}
	r := e.series(60).Patches[0].Run(state.RunPush, "Build")
	if r.Result != "" || r.MinAttempt != 1 {
		t.Fatalf("run = %+v", r)
	}
	e.step()
	if r.Result != "" {
		t.Fatal("old attempt must be ignored")
	}
	e.gh.restartRun(ids[0])
	e.gh.finishRun(ids[0], "cancelled")
	e.step()
	if len(e.gh.reruns) != 1 || r.Result != state.ResultWarning || r.Attempt != 2 {
		t.Fatalf("run = %+v reruns = %v", r, e.gh.reruns)
	}
}

func TestPushOffMonitorsWithoutRerunning(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(60, 1, "lorem", e.now)
	e.step()
	ids := e.gh.startRuns("series_60", "sha-6001", "Build")
	e.gh.finishRun(ids[0], "cancelled")
	e.cfg.Mode.Push = "off"
	e.step()
	if len(e.gh.reruns) != 0 {
		t.Fatalf("push mode off started reruns: %v", e.gh.reruns)
	}
	r := e.series(60).Patches[0].Run(state.RunPush, "Build")
	if r == nil || r.Result != state.ResultWarning || r.Final.Email != state.StepSent {
		t.Fatalf("cancelled run was not monitored and reported: %+v", r)
	}
}

func TestPushOffDefersPendingDispatches(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(60, 1, "lorem", e.now)
	e.step()
	pt := e.series(60).Patches[0]
	r := &state.Run{
		Kind: state.RunDispatch, File: "leadtest-lint.yml",
		Status: "dispatching", CorrelationID: "pending-dispatch",
	}
	pt.Runs = append(pt.Runs, r)
	e.cfg.Mode.Push = "off"
	e.step()
	e.advance(e.cfg.Limits.RunTimeout.D() + time.Hour)
	e.step()
	if len(e.gh.dispatches) != 0 || r.Status != "dispatching" || r.Failure != nil ||
		r.Result != "" {
		t.Fatalf("push mode off changed pending dispatch: %+v, %v", r, e.gh.dispatches)
	}
	e.cfg.Mode.Push = "live"
	e.step()
	if len(e.gh.dispatches) != 1 || r.Status != "requested" || r.Result != "" {
		t.Fatalf("dispatch did not resume in live mode: %+v, %v", r, e.gh.dispatches)
	}
	if pt.Pushed != state.FormatTime(e.now) {
		t.Errorf("timeouts were not restarted by the delayed dispatch: %s", pt.Pushed)
	}
	e.advance(e.cfg.Limits.NoRunTimeout.D() + time.Minute)
	e.step()
	if r.Result != state.ResultWarning {
		t.Errorf("restarted timeout did not expire: %+v", r)
	}
}

func TestEmailFailureRetried(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(70, 1, "lorem", e.now)
	e.mail.fail = 1
	e.step()
	r := e.series(70).Patches[0].Runs[0]
	if r.Final.Email != "" || r.Failure == nil {
		t.Fatalf("run = %+v", r)
	}
	e.advance(6 * time.Minute)
	e.step()
	if r.Final.Email != state.StepSent || r.Failure != nil || len(e.mail.sent) != 1 {
		t.Fatalf("run = %+v", r)
	}
}

func TestTestModeNeverCcs(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Email = "test"
	e.cfg.Mode.AllowedRecipients = []string{"lorem-build@lists.example.org"}
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.repo.failAt[80] = 0
	e.step()
	if len(e.mail.sent) != 1 {
		t.Fatalf("sent = %d", len(e.mail.sent))
	}
	m := e.mail.sent[0]
	data := string(m.Data)
	if len(m.Recipients) != 1 || strings.Contains(data, "Cc:") ||
		!strings.Contains(data, "Subject: [leadtest-dryrun] |fail|") ||
		strings.Contains(data, "Test-Label: ") {
		t.Errorf("test mail:\n%s", data)
	}
}

func TestEmailOffAndChecksLog(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Email = "off"
	e.cfg.Mode.Checks = "log"
	e.pw.addSeries(81, 1, "lorem", e.now)
	e.step()
	e.gh.startRuns("series_81", "sha-8101", "Build")
	e.step()
	r := e.series(81).Patches[0].Run(state.RunPush, "Build")
	if len(e.mail.sent) != 0 || len(e.pw.posted) != 0 ||
		r.Pending.Check != state.StepLogged || r.Pending.Email != state.StepSkipped {
		t.Fatalf("run = %+v sent = %d posted = %d", r, len(e.mail.sent), len(e.pw.posted))
	}
}

func TestPushOff(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Push = "off"
	e.pw.addSeries(82, 1, "lorem", e.now)
	e.step()
	if s := e.series(82); s.Status != state.SeriesQueued || len(e.gh.branches) != 0 {
		t.Fatalf("series = %+v", s)
	}
}

func reportBody(label, status string, patchID int) string {
	return fmt.Sprintf("Test-Label: %s\nTest-Status: %s\n"+
		"https://pw.example.org/patch/%d/\n\n_lab: %s_\n", label, status, patchID, status)
}

func TestIngest(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Push = "off"
	prev := e.now.AddDate(0, -1, 0)
	// Messages present before the first scan are only marked as seen.
	e.reports.add(prev, 1, "|SUCCESS| pw5 old", reportBody("lab", "SUCCESS", 5), "<a@lab>")
	e.step()
	if len(e.reports.fetched) != 0 {
		t.Fatalf("fetched on first scan: %v", e.reports.fetched)
	}
	if c := e.p.Store.Meta.Archives["reports"]; c.LastMonth != "2026-03" ||
		c.Months[archive.MonthKey(prev.Year(), prev.Month())] != 1 {
		t.Fatalf("cursor = %+v", c)
	}

	e.reload()
	e.pw.addSeries(90, 1, "lorem", e.now)
	other := *e.pw.patches[9001]
	other.ID = 9999
	other.Project.LinkName = "other"
	e.pw.patches[9999] = &other

	u1 := e.reports.add(prev, 2, "|FAILURE| pw9001 x",
		reportBody("lab-robot: Perf test", "FAILURE", 9001), "<b@lab>")
	e.reports.add(e.now, 3, "|fail| pw9999 x", reportBody("lab", "fail", 9999), "<c@lab>")
	e.reports.add(e.now, 4, "hello", "Just a discussion.\n", "<d@lab>")
	ownPending := "<leadtest.90.9001.github-robot-build.pending.1.1@example.org>"
	e.reports.add(e.now, 5, "|pending| pw9001 x",
		reportBody("github-robot: Build", "pending", 9001), ownPending)
	u6 := e.reports.add(e.now, 6, "|success| pw9001 x",
		reportBody("github-robot: Build", "success", 9001),
		"<leadtest.90.9001.github-robot-build.final.1.1@example.org>")
	e.reports.fail[u6] = 1
	e.step()
	if len(e.pw.posted) != 1 || e.pw.posted[0].patch != 9001 ||
		e.pw.posted[0].check.Context != "lab-robot-_Perf_test" ||
		e.pw.posted[0].check.State != "fail" || e.pw.posted[0].check.TargetURL != u1 {
		t.Fatalf("posted = %+v", e.pw.posted)
	}
	c := e.p.Store.Meta.Archives["reports"]
	if c.Retry[u6] == nil {
		t.Fatalf("failed message not queued for retry: %+v", c)
	}

	// The retry posts the check; reading the same messages again does
	// not post duplicates.
	e.reload()
	c = e.p.Store.Meta.Archives["reports"]
	e.advance(10 * time.Minute)
	e.p.Store.Meta.Archives["reports"].Months[archive.MonthKey(prev.Year(), prev.Month())] = 0
	e.step()
	if len(e.pw.posted) != 2 || e.pw.posted[1].check.Context != "github-robot-_Build" ||
		len(c.Retry) != 0 {
		t.Fatalf("posted = %+v retry = %v", e.pw.posted, c.Retry)
	}
}

func TestIngestMonthCatchUp(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Push = "off"
	e.step()
	e.reload()
	e.pw.addSeries(91, 1, "lorem", e.now)
	e.now = e.now.AddDate(0, 3, 0)
	old := e.now.AddDate(0, -2, 0)
	e.reports.add(old, 10, "|fail| pw9101 x", reportBody("lab", "fail", 9101), "<x@lab>")
	e.step()
	if len(e.pw.posted) != 1 {
		t.Fatalf("posted = %+v", e.pw.posted)
	}
}

func TestRecheckFromArchive(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Report.ApplySuccess = false
	e.pw.addSeries(100, 1, "lorem", e.now)
	e.step()
	ids := e.gh.startRuns("series_100", "sha-10001", "Build")
	e.gh.finishRun(ids[0], "failure")
	e.step()
	e.advance(11 * time.Minute)
	e.step()
	if s := e.series(100); s.Status != state.SeriesDone {
		t.Fatalf("series = %+v", s)
	}
	sent := len(e.mail.sent)

	e.reload()
	e.dev.add(e.now, 50, "Re: [lorem-dev,1/1] lorem: Patch 1 of series 100", "", "")
	e.pw.comments[10001] = comments(
		"Thanks.\n\n> Recheck-request: github-robot\n",
		"Recheck-request: lorem-lab, github-robot\n")
	e.step()
	if len(e.gh.reruns) != 1 || e.gh.reruns[0] != ids[0] {
		t.Fatalf("reruns = %v", e.gh.reruns)
	}
	s := e.series(100)
	pt := s.Patches[0]
	if s.Status != state.SeriesTesting || e.p.Store.Find(100) == nil ||
		pt.Status != state.PatchPushed ||
		pt.CommentsSeen != 2 {
		t.Fatalf("series = %+v patch = %+v", s, pt)
	}
	e.gh.restartRun(ids[0])
	e.gh.finishRun(ids[0], "success")
	e.step()
	if len(e.mail.sent) != sent+1 {
		t.Fatalf("sent = %d, want %d", len(e.mail.sent), sent+1)
	}
	if !strings.Contains(string(e.mail.sent[sent].Data), "|success| pw10001") {
		t.Errorf("recheck report:\n%s", e.mail.sent[sent].Data)
	}
	// The same comment does not trigger another recheck.
	e.dev.add(e.now, 51, "Re: [lorem-dev,1/1] lorem: Patch 1 of series 100", "", "")
	e.step()
	if len(e.gh.reruns) != 1 {
		t.Errorf("reruns = %v", e.gh.reruns)
	}
}

func TestDependsOn(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(110, 1, "lorem", e.now)
	e.pw.addSeries(111, 1, "ipsum", e.now)
	e.pw.patches[11101].Content = "Lorem.\n\nDepends-on: series-110\n"
	e.cfg.Limits.MaxInflightPatches = 1
	e.cfg.Limits.PerAuthorMin = 1
	e.step()
	if s := e.series(111); s.Status != state.SeriesQueued {
		t.Fatalf("series 111 = %+v", s)
	}
	e.cfg.Limits.MaxInflightPatches = 10
	e.step()
	s := e.series(111)
	if s.Status != state.SeriesTesting || s.Base.SHA != "sha-11001" || s.DependsOn != 110 {
		t.Fatalf("series 111 = %+v base = %+v", s, s.Base)
	}
}

func TestDependencyWaits(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Push = "live"
	e.pw.addSeries(120, 1, "lorem", e.now)
	e.p.Store.Add(&state.Series{ID: 119, Status: state.SeriesNew,
		Discovered: state.FormatTime(e.now),
		Patches:    []*state.Patch{}, Hold: true})
	e.pw.patches[12001].Content = "Depends-on: patch-11901\n"
	dep := *e.pw.patches[12001]
	dep.ID = 11901
	dep.Series = []pw.SeriesSummary{{ID: 119}}
	e.pw.patches[11901] = &dep
	e.step()
	if s := e.series(120); s.Status != state.SeriesWaiting ||
		s.Reason != "waiting for series 119" {
		t.Fatalf("series = %+v", s)
	}
	e.advance(4 * 24 * time.Hour)
	e.pw.series[120].Date = e.now.Format("2006-01-02T15:04:05")
	e.step()
	if s := e.series(120); s.Status != state.SeriesTesting ||
		!strings.Contains(s.Reason, "not available") {
		t.Fatalf("series = %+v", s)
	}
}

func TestManualFlags(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Push = "off"
	e.pw.addSeries(130, 1, "lorem", e.now)
	e.pw.addSeries(131, 1, "ipsum", e.now)
	e.step()
	e.series(130).Skip = "lorem reason"
	e.step()
	if s := e.series(130); s.Status != state.SeriesSkipped ||
		s.Reason != "manual: lorem reason" {
		t.Fatalf("series = %+v", s)
	}
	e.reload()
	s := e.series(130)
	if err := e.p.Store.Reopen(s); err != nil {
		t.Fatal(err)
	}
	s.Retry = true
	e.step()
	if s := e.series(130); s.Status != state.SeriesQueued || s.Retry || s.Skip != "" {
		t.Fatalf("series = %+v", s)
	}
}

func TestCustomWorkflows(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Workflows.Custom = true
	e.cfg.Workflows.CustomPerSeries = []string{"leadtest-series.yml"}
	e.cfg.Report.ApplySuccess = false
	e.gh.files["leadtest-checkpatch.yml"] =
		"name: Checkpatch\non:\n  workflow_dispatch:\n    inputs: {}\n"
	e.gh.files["leadtest-series.yml"] = "on: [workflow_dispatch]\n"
	e.gh.files["leadtest-other.yml"] = "on: push\n"
	e.gh.files["build-and-test.yml"] = "on: [push, workflow_dispatch]\n"
	e.pw.addSeries(140, 2, "lorem", e.now)
	e.step()
	if len(e.gh.dispatches) != 3 {
		t.Fatalf("dispatches = %v", e.gh.dispatches)
	}
	d := e.gh.dispatches[0]
	if d["file"] != "leadtest-checkpatch.yml" || d["ref"] != "main" ||
		d["sha"] != "sha-14001" ||
		d["patch_index"] != "1" ||
		d["correlation_id"] != "140-14001-leadtest-checkpatch-1" {
		t.Errorf("dispatch = %v", d)
	}
	e.gh.dispatched = []gh.WorkflowRun{{ID: 5000, Name: "Checkpatch",
		DisplayTitle: "leadtest 140-14001-leadtest-checkpatch-1", Status: "completed",
		Conclusion: "failure",
		RunAttempt: 1, HTMLURL: "https://github.com/ipsum/lorem/actions/runs/5000"}}
	e.step()
	r := runByFile(e.series(140).Patches[0], "leadtest-checkpatch.yml")
	if r.Result != state.ResultFail || r.Workflow != "Checkpatch" ||
		r.Final.Email != state.StepSent {
		t.Fatalf("run = %+v", r)
	}
	if !strings.Contains(string(e.mail.sent[0].Data), "Test-Label: github-robot: Checkpatch") {
		t.Errorf("mail:\n%s", e.mail.sent[0].Data)
	}
}

func TestForeignDispatchedRunsIgnored(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Workflows.Custom = true
	e.cfg.Report.ApplySuccess = false
	e.p.Actor = func() string { return "lorem-app[bot]" }
	e.gh.files["leadtest-checkpatch.yml"] = "name: Checkpatch\non: workflow_dispatch\n"
	// A workflow of another patch dispatched a run with the name the
	// bot will use, before the bot does.
	forged := gh.WorkflowRun{ID: 6000, Name: "Checkpatch",
		DisplayTitle: "leadtest 141-14101-leadtest-checkpatch-1", Status: "completed",
		Conclusion: "success", RunAttempt: 1,
		Actor: gh.User{Login: "github-actions[bot]"}}
	e.gh.dispatched = []gh.WorkflowRun{forged}
	e.pw.addSeries(141, 1, "lorem", e.now)
	e.step()
	if len(e.gh.dispatches) != 1 {
		t.Fatalf("a foreign run kept the bot from dispatching: %v", e.gh.dispatches)
	}
	e.step()
	r := runByFile(e.series(141).Patches[0], "leadtest-checkpatch.yml")
	if r.ID != 0 || r.Result != "" {
		t.Fatalf("a foreign run was taken as the result: %+v", r)
	}
	own := forged
	own.ID, own.Conclusion = 6001, "failure"
	own.Actor = gh.User{Login: "lorem-app[bot]"}
	e.gh.dispatched = append(e.gh.dispatched, own)
	e.step()
	if r.ID != 6001 || r.Result != state.ResultFail {
		t.Fatalf("the bot's own run was not used: %+v", r)
	}
}

func TestDispatchFailureRetried(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Workflows.Custom = true
	e.gh.files["leadtest-checkpatch.yml"] = "on: workflow_dispatch\n"
	e.gh.fail["dispatch"] = 1
	e.pw.addSeries(150, 1, "lorem", e.now)
	e.step()
	if len(e.gh.dispatches) != 0 {
		t.Fatalf("dispatches = %v", e.gh.dispatches)
	}
	e.advance(6 * time.Minute)
	e.step()
	if len(e.gh.dispatches) != 1 {
		t.Fatalf("dispatches = %v", e.gh.dispatches)
	}
}

func TestGC(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Report.ApplySuccess = false
	e.pw.addSeries(160, 1, "lorem", e.now)
	e.step()
	e.gh.finishRun(e.gh.startRuns("series_160", "sha-16001", "Build")[0], "success")
	e.step()
	e.advance(20 * time.Minute)
	e.step()
	if e.series(160).Status != state.SeriesDone {
		t.Fatal("series not done")
	}
	e.gh.refs = map[string]string{
		"refs/heads/series_160": "x", "refs/leadtest/series_160": "x",
		"refs/heads/series_5":       "x",
		"refs/leadtest/patch_16001": "x", "refs/leadtest/patch_5": "x",
	}
	finished := e.now
	e.now = e.now.AddDate(0, 2, 0)
	e.reload()
	e.step()
	if len(e.gh.deleted) != 0 {
		t.Fatalf("refs deleted before the retention period expired: %v", e.gh.deleted)
	}
	e.now = finished.Add(e.cfg.Limits.BranchRetention.D() + 24*time.Hour)
	e.reload()
	e.step()
	if strings.Join(e.gh.deleted,
		",") != "heads/series_160,leadtest/series_160,leadtest/patch_16001" {
		t.Errorf("deleted = %v", e.gh.deleted)
	}
}

func TestParseRecheck(t *testing.T) {
	labels := []string{"github-robot"}
	tests := []struct {
		content string
		all     bool
		names   string
	}{
		{"x\nRecheck-request: lab, github-robot\n", true, "[lab]"},
		{"> Recheck-request: github-robot\n", false, "[]"},
		{"Recheck-request: GitHub-Robot\n", true, "[]"},
		{"Recheck-request: github-robot-_Build_and_Test, iol-unit\n", false,
			"[github-robot-_Build_and_Test iol-unit]"},
		{"Lorem ipsum.\n", false, "[]"},
	}
	for _, tt := range tests {
		all, names := ParseRecheck(tt.content, labels)
		if all != tt.all || fmt.Sprint(names) != tt.names {
			t.Errorf("%q: all=%v names=%v", tt.content, all, names)
		}
	}
}

func TestHasDispatchTrigger(t *testing.T) {
	for content, want := range map[string]bool{
		"on: workflow_dispatch\n":                  true,
		"on: [push, workflow_dispatch]\n":          true,
		"on:\n  workflow_dispatch:\n  push:\n":     true,
		"on: push\n":                               false,
		"name: x\njobs: {}\n":                      false,
		"not yaml: [":                              false,
		"true:\n  workflow_dispatch: {}\n":         true,
		"on:\n  push:\n    branches: [dispatch]\n": false,
	} {
		if got := hasDispatchTrigger([]byte(content)); got != want {
			t.Errorf("%q: got %v", content, got)
		}
	}
}

func TestFirstRunDoesNotTestOldSeries(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Limits.InitialBackfill = 0
	e.pw.addSeries(170, 1, "lorem", e.now.Add(-2*time.Hour))
	e.step()
	if s := e.p.Store.Find(170); s != nil || len(e.gh.branches) != 0 {
		t.Fatalf("old series tracked: %+v", s)
	}
	if e.p.Store.Meta.DiscoveryStart != state.FormatTime(e.now) ||
		e.p.Store.Meta.Events["series-completed"] != 1 {
		t.Fatalf("meta = %+v", e.p.Store.Meta)
	}

	// A new run must not pick the old series up through the series list.
	e.reload()
	e.advance(time.Hour)
	e.pw.addSeries(171, 1, "ipsum", e.now)
	e.step()
	if e.p.Store.Find(170) != nil || e.series(171).Status != state.SeriesTesting {
		t.Fatalf("queue = %+v", e.p.Store.Queue)
	}
}

func TestFirstRunWithoutEvents(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Limits.InitialBackfill = 0
	e.step()
	e.advance(time.Hour)
	e.pw.addSeries(180, 1, "lorem", e.now)
	e.step()
	if e.series(180).Status != state.SeriesTesting {
		t.Fatalf("series = %+v", e.series(180))
	}
}

func TestInitialBackfill(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Limits.InitialBackfill = config.Duration(3 * time.Hour)
	e.pw.addSeries(190, 1, "lorem", e.now.Add(-5*time.Hour))
	e.pw.addSeries(191, 1, "ipsum", e.now.Add(-2*time.Hour))
	e.step()
	if e.p.Store.Find(190) != nil || e.series(191).Status != state.SeriesTesting {
		t.Fatalf("queue = %+v", e.p.Store.Queue)
	}
}

// loseState starts over with an empty state, as after a killed run.
func (e *env) loseState() {
	e.dir = e.t.TempDir()
	e.p = nil
	e.reload()
}

func TestLostStateDoesNotStartRunsAgain(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Workflows.Custom = true
	e.gh.files["leadtest-checkpatch.yml"] = "on: workflow_dispatch\n"
	e.pw.addSeries(200, 2, "lorem", e.now)
	e.step()
	if e.gh.setBranch != 2 || len(e.gh.dispatches) != 2 || e.repo.applies != 1 {
		t.Fatalf("set-branch=%d dispatches=%d applies=%d", e.gh.setBranch,
			len(e.gh.dispatches), e.repo.applies)
	}
	for _, d := range e.gh.dispatches {
		e.gh.dispatched = append(e.gh.dispatched,
			gh.WorkflowRun{ID: 9000, Name: "Checkpatch",
				DisplayTitle: "leadtest " + d["correlation_id"],
				Status:       "in_progress", RunAttempt: 1})
	}

	e.loseState()
	e.advance(time.Minute)
	e.step()
	s := e.series(200)
	if e.repo.applies != 1 || e.gh.setBranch != 2 || len(e.gh.dispatches) != 2 {
		t.Fatalf("set-branch=%d dispatches=%d applies=%d", e.gh.setBranch,
			len(e.gh.dispatches), e.repo.applies)
	}
	if s.Status != state.SeriesTesting || s.Base.SHA != "base-main" ||
		s.Patches[0].SHA != "sha-20001" || s.Patches[1].SHA != "sha-20002" ||
		s.Patches[0].Status != state.PatchPushed ||
		s.Patches[1].Status != state.PatchPushed {
		t.Fatalf("series = %+v %+v %+v", s, s.Patches[0], s.Patches[1])
	}
	if r := runByFile(s.Patches[1], "leadtest-checkpatch.yml"); r == nil ||
		r.Status != "requested" {
		t.Fatalf("dispatch run = %+v", r)
	}
}

func TestLostStateReusesAppliedPrefix(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(210, 3, "lorem", e.now)
	e.repo.failAt[210] = 1
	e.step()
	if e.gh.branches["series_210"] != "sha-21001" || e.gh.setBranch != 1 {
		t.Fatalf("branches = %v", e.gh.branches)
	}

	e.loseState()
	delete(e.repo.failAt, 210)
	e.step()
	s := e.series(210)
	if s.Patches[0].SHA != "sha-21001" || s.Patches[1].SHA != "sha-21002.2" ||
		s.Patches[2].SHA != "sha-21003.2" || e.repo.applied != 3 {
		t.Fatalf("patches = %+v %+v %+v applied=%d", s.Patches[0], s.Patches[1],
			s.Patches[2], e.repo.applied)
	}
	// Patch 1 was already on the branch; only the new patches move it.
	if e.gh.setBranch != 3 || e.gh.branches["series_210"] != "sha-21003.2" {
		t.Fatalf("set-branch = %d branches = %v", e.gh.setBranch, e.gh.branches)
	}
}

func TestBranchAheadOfState(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Limits.MaxInflightPatches = 2
	e.cfg.Limits.PerAuthorMaxShare = 1
	e.pw.addSeries(220, 3, "lorem", e.now)
	e.step()
	s := e.series(220)
	if s.Patches[1].Status != state.PatchPushed || s.Patches[2].Status != state.PatchApplied {
		t.Fatalf("patches = %+v %+v", s.Patches[1], s.Patches[2])
	}
	// The branch moved further than the state knows.
	e.gh.branches["series_220"] = "sha-22003"
	n := e.gh.setBranch
	e.cfg.Limits.MaxInflightPatches = 10
	e.step()
	if s.Patches[2].Status != state.PatchPushed || e.gh.setBranch != n {
		t.Fatalf("patch = %+v set-branch = %d", s.Patches[2], e.gh.setBranch)
	}
}

func TestRetryAppliesAgain(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Report.ApplySuccess = false
	e.pw.addSeries(230, 1, "lorem", e.now)
	e.step()
	e.gh.finishRun(e.gh.startRuns("series_230", "sha-23001", "Build")[0], "success")
	e.step()
	e.advance(20 * time.Minute)
	e.step()
	s := e.series(230)
	if s.Status != state.SeriesDone {
		t.Fatalf("series = %+v", s)
	}
	if err := e.p.Store.Reopen(s); err != nil {
		t.Fatal(err)
	}
	s.Retry = true
	e.step()
	if e.repo.applies != 2 || s.Patches[0].SHA != "sha-23001.2" || s.Reapply ||
		e.gh.branches["series_230"] != "sha-23001.2" {
		t.Fatalf("series = %+v patch = %+v", s, s.Patches[0])
	}
}

// testedPatch returns a done patch with a successful and a failed run.
func testedPatch(t *testing.T, e *env, id int) (ok, failed int64) {
	t.Helper()
	e.cfg.Report.ApplySuccess = false
	e.pw.addSeries(id, 1, "lorem", e.now)
	e.step()
	sha := fmt.Sprintf("sha-%d01", id)
	ids := e.gh.startRuns(fmt.Sprintf("series_%d", id), sha, "Build", "Test")
	e.gh.finishRun(ids[0], "success")
	e.gh.finishRun(ids[1], "failure")
	e.step()
	e.advance(11 * time.Minute)
	e.step()
	if s := e.series(id); s.Status != state.SeriesDone {
		t.Fatalf("series = %+v", s)
	}
	return ids[0], ids[1]
}

func TestRecheckRerunsOnlyFailedJobs(t *testing.T) {
	e := newEnv(t, "")
	ok, failed := testedPatch(t, e, 240)
	s := e.series(240)
	s.Patches[0].Recheck = true
	sent := len(e.mail.sent)
	e.step()
	if fmt.Sprint(e.gh.reruns) != fmt.Sprint([]int64{failed}) ||
		fmt.Sprint(e.gh.failedOnly) != "[true]" {
		t.Fatalf("reruns = %v failed only = %v", e.gh.reruns, e.gh.failedOnly)
	}
	pt := e.series(240).Patches[0]
	if r := pt.Run(state.RunPush, "Build"); r.ID != ok || r.Result != state.ResultSuccess ||
		r.Final.Email != state.StepSent {
		t.Errorf("successful run must be kept: %+v", r)
	}
	e.gh.restartRun(failed)
	e.gh.finishRun(failed, "success")
	e.step()
	e.advance(11 * time.Minute)
	e.step()
	if len(e.mail.sent) != sent+1 || e.series(240).Status != state.SeriesDone {
		t.Fatalf("sent = %d, want %d; series = %+v", len(e.mail.sent),
			sent+1, e.series(240))
	}
}

func TestRerunFallsBackToAllJobs(t *testing.T) {
	e := newEnv(t, "")
	_, failed := testedPatch(t, e, 250)
	e.gh.refuseFailedOnly = true
	e.series(250).Patches[0].Recheck = true
	e.step()
	if fmt.Sprint(e.gh.reruns) != fmt.Sprint([]int64{failed}) ||
		fmt.Sprint(e.gh.failedOnly) != "[false]" {
		t.Fatalf("reruns = %v failed only = %v", e.gh.reruns, e.gh.failedOnly)
	}
}

func TestRerunAllJobs(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Workflows.RerunJobs = "all"
	ok, failed := testedPatch(t, e, 260)
	e.series(260).Patches[0].Recheck = true
	e.step()
	if fmt.Sprint(e.gh.reruns) != fmt.Sprint([]int64{ok, failed}) ||
		fmt.Sprint(e.gh.failedOnly) != "[false false]" {
		t.Fatalf("reruns = %v failed only = %v", e.gh.reruns, e.gh.failedOnly)
	}
}

// requestRecheck adds a Recheck-request comment to the patch of a series
// and marks its comments for fetching, as the dev list scan would.
func requestRecheck(e *env, seriesID int, line string) {
	pt := e.series(seriesID).Patches[0]
	e.pw.comments[pt.ID] = append(e.pw.comments[pt.ID],
		pw.Comment{ID: len(e.pw.comments[pt.ID]) + 1,
			Content: "Thanks.\n\nRecheck-request: " + line + "\n"})
	pt.CommentsPending = true
}

func TestRecheckByCheckName(t *testing.T) {
	e := newEnv(t, "")
	ok, failed := testedPatch(t, e, 270)
	requestRecheck(e, 270, "github-robot-_Build")
	e.step()
	// An explicitly named run is rerun even though it succeeded, as a
	// whole, since it has no failed jobs.
	if fmt.Sprint(e.gh.reruns) != fmt.Sprint([]int64{ok}) ||
		fmt.Sprint(e.gh.failedOnly) != "[false]" {
		t.Fatalf("reruns = %v failed only = %v", e.gh.reruns, e.gh.failedOnly)
	}
	pt := e.series(270).Patches[0]
	if r := pt.Run(state.RunPush, "Test"); r.ID != failed || r.Result != state.ResultFail {
		t.Errorf("unrequested run changed: %+v", r)
	}
	if pt.Recheck || pt.RecheckChecks != nil || pt.CommentsSeen != 1 {
		t.Errorf("patch = %+v", pt)
	}
}

func TestRecheckSeveralNames(t *testing.T) {
	e := newEnv(t, "")
	ok, failed := testedPatch(t, e, 280)
	requestRecheck(e, 280, "iol-unit-testing, github-robot-_Test github-robot-_build")
	e.step()
	if fmt.Sprint(e.gh.reruns) != fmt.Sprint([]int64{ok, failed}) ||
		fmt.Sprint(e.gh.failedOnly) != "[false true]" {
		t.Fatalf("reruns = %v failed only = %v", e.gh.reruns, e.gh.failedOnly)
	}
}

func TestRecheckUnknownNameIgnored(t *testing.T) {
	e := newEnv(t, "")
	testedPatch(t, e, 290)
	requestRecheck(e, 290, "iol-unit-testing github-robot-_Lorem")
	e.step()
	if len(e.gh.reruns) != 0 || e.series(290).Status != state.SeriesDone {
		t.Fatalf("reruns = %v", e.gh.reruns)
	}
}

func TestRecheckCustomWorkflows(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Workflows.Custom = true
	e.cfg.Report.ApplySuccess = false
	e.gh.files["leadtest-checkpatch.yml"] = "on: workflow_dispatch\n"
	e.gh.files["leadtest-perf.yml"] = "on: workflow_dispatch\n"
	e.pw.addSeries(300, 1, "lorem", e.now)
	e.step()
	e.gh.dispatched = []gh.WorkflowRun{{ID: 7000, Name: "Checkpatch",
		DisplayTitle: "leadtest 300-30001-leadtest-checkpatch-1", Status: "completed",
		Conclusion: "failure",
		RunAttempt: 1}}
	e.gh.finishRun(e.gh.startRuns("series_300", "sha-30001", "Build")[0], "success")
	e.step()
	// perf.yml never starts and is reported as a warning.
	e.advance(61 * time.Minute)
	e.step()
	e.advance(11 * time.Minute)
	e.step()
	if r := runByFile(e.series(300).Patches[0],
		"leadtest-perf.yml"); r.Result != state.ResultWarning {
		t.Fatalf("perf run = %+v", r)
	}
	if s := e.series(300); s.Status != state.SeriesDone {
		t.Fatalf("series = %+v", s)
	}

	requestRecheck(e, 300, "github-robot-_Checkpatch github-robot-_leadtest-perf.yml")
	e.step()
	if fmt.Sprint(e.gh.reruns) != "[7000]" {
		t.Fatalf("reruns = %v", e.gh.reruns)
	}
	last := e.gh.dispatches[len(e.gh.dispatches)-1]
	if len(e.gh.dispatches) != 3 || last["file"] != "leadtest-perf.yml" ||
		last["correlation_id"] != "300-30001-leadtest-perf-2" {
		t.Fatalf("dispatches = %v", e.gh.dispatches)
	}
	pt := e.series(300).Patches[0]
	if r := pt.Run(state.RunPush, "Build"); r.Result != state.ResultSuccess {
		t.Errorf("Build changed: %+v", r)
	}
	if r := runByFile(pt, "leadtest-perf.yml"); r.Status != "requested" || r.Result != "" {
		t.Errorf("perf run = %+v", r)
	}
}

func TestNextCorrelationID(t *testing.T) {
	for in, want := range map[string]string{"1-2-x-1": "1-2-x-2",
		"1-2-x-9": "1-2-x-10", "odd": "odd-2"} {
		if got := nextCorrelationID(in); got != want {
			t.Errorf("%q: got %q", in, got)
		}
	}
}

func TestSyncTools(t *testing.T) {
	e := newEnv(t, "")
	e.step()
	e.step()
	if fmt.Sprint(e.gh.merges) != "[main]" ||
		e.p.Store.Meta.ToolsSynced != state.FormatTime(e.now) {
		t.Fatalf("merges = %v synced = %q", e.gh.merges, e.p.Store.Meta.ToolsSynced)
	}
	e.reload()
	e.advance(24 * time.Hour)
	e.step()
	if len(e.gh.merges) != 1 {
		t.Fatalf("synced again too early: %v", e.gh.merges)
	}
	e.reload()
	e.advance(7 * 24 * time.Hour)
	e.step()
	if len(e.gh.merges) != 2 {
		t.Fatalf("merges = %v", e.gh.merges)
	}
}

func TestSyncToolsErrors(t *testing.T) {
	e := newEnv(t, "")
	e.gh.mergeErr = errTransient
	e.step()
	if e.p.Store.Meta.ToolsSynced != "" || len(e.p.Stats.Errors) != 0 {
		t.Fatalf("transient error: synced = %q errors = %v",
			e.p.Store.Meta.ToolsSynced, e.p.Stats.Errors)
	}
	e.reload()
	e.gh.mergeErr = &httpx.StatusError{Code: 409}
	e.step()
	if len(e.gh.merges) != 2 || e.p.Store.Meta.ToolsSynced == "" || len(e.p.Stats.Errors) != 1 {
		t.Fatalf("conflict: merges = %v synced = %q errors = %v",
			e.gh.merges, e.p.Store.Meta.ToolsSynced, e.p.Stats.Errors)
	}
}

func TestSyncToolsDisabled(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Push = "off"
	e.step()
	e.cfg.Mode.Push = "live"
	e.cfg.TestRepo.SyncInterval = 0
	e.reload()
	e.step()
	if len(e.gh.merges) != 0 {
		t.Fatalf("merges = %v", e.gh.merges)
	}
}

func TestArchivesReadOncePerRun(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Push = "off"
	e.step()
	e.step()
	e.step()
	if n := e.reports.monthFetches; n != 2 {
		t.Errorf("reports archive months fetched %d times in one run, want 2", n)
	}
	if n := e.dev.monthFetches; n != 2 {
		t.Errorf("dev archive months fetched %d times in one run, want 2", n)
	}
	e.reload()
	e.step()
	if n := e.reports.monthFetches; n != 4 {
		t.Errorf("reports archive months fetched %d times after a new run, want 4", n)
	}
}

func TestArchiveReadRetriedAfterFailure(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Mode.Push = "off"
	e.reports.monthFail = 1
	e.step()
	e.step()
	if n := e.reports.monthFetches; n != 3 ||
		e.p.Store.Meta.Archives["reports"].LastMonth == "" {
		t.Errorf("fetches = %d cursor = %+v", n, e.p.Store.Meta.Archives["reports"])
	}
}
