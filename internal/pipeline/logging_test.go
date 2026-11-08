// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"leadtestkit/internal/config"
	"leadtestkit/internal/gh"
	"leadtestkit/internal/logx"
	"leadtestkit/internal/state"
)

// urlThenPunct finds URLs that terminals would extend with punctuation.
var urlThenPunct = regexp.MustCompile(`https?://\S*[.:;,)](\s|$)`)

func requireLogs(t *testing.T, buf *bytes.Buffer, messages ...string) {
	t.Helper()
	for _, message := range messages {
		if !strings.Contains(buf.String(), message) {
			t.Errorf("missing log %q in:\n%s", message, buf.String())
		}
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if urlThenPunct.MatchString(line) {
			t.Errorf("a URL is followed by punctuation in %q", line)
		}
	}
}

func TestLogsExplainDiscoveryWindow(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.cfg.Limits.InitialBackfill = config.Duration(2 * time.Hour)
	e.pw.addSeries(7, 1, "lorem", e.now.Add(-time.Hour))
	e.pw.addSeries(8, 1, "ipsum", e.now.Add(-3*time.Hour))
	e.p.discover(context.Background())
	requireLogs(t, &buf,
		"First discovery: recording",
		"Initial backfill: 2h0m0s.",
		"limited to 3 event pages",
		"not an exhaustive historical scan",
		"Found series 7 (lorem: Series 7); checking it for eligibility next.",
		"1 from before the cutoff 2026-03-10T10:00:00Z, not added (8)",
		"newest 50 series",
	)
	if e.p.Store.Find(7) == nil || e.p.Store.Find(8) != nil {
		t.Fatal("discovery did not respect its cutoff")
	}

	buf.Reset()
	e.p.Store.Meta.Events[seriesEvents] = 123
	e.cfg.Limits.InitialBackfill = config.Duration(24 * time.Hour)
	e.p.discover(context.Background())
	requireLogs(t, &buf,
		"Resuming after series event 123",
		"Changing initial_backfill does not rewind",
		"already ran in this invocation",
	)
}

func TestLogsExplainZeroBackfill(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.cfg.Limits.InitialBackfill = 0
	e.pw.addSeries(7, 1, "lorem", e.now.Add(-time.Hour))
	e.p.discover(context.Background())
	requireLogs(t, &buf, "Initial backfill is zero",
		"Safety scan of the newest 1 series: 1 from before the cutoff "+
			state.FormatTime(e.now)+", not added (7).")
	if e.p.Store.Find(7) != nil {
		t.Fatal("zero backfill picked up an older series")
	}
}

func TestLogsExplainFinalCycle(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.p.Step(context.Background(), true)
	requireLogs(t, &buf,
		"eligible; queued 1 patches",
		"Not scheduling new tests in the final polling cycle",
	)
	if len(e.gh.branches) != 0 || e.p.Store.Find(7).Status != state.SeriesQueued {
		t.Fatal("final cycle unexpectedly scheduled a patch")
	}
}

func TestLogsNarrateTestLifecycle(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.step()
	requireLogs(t, &buf,
		"Starting polling cycle 1",
		"Looking for new series in Patchwork",
		"eligible; queued 1 patches",
		"CI capacity:",
		"Selected one patch from series 7",
		"selected base main",
		"applied as commit sha-701",
		"updating branch series_7",
		"waiting for GitHub workflows",
		"sending the final report for apply-robot",
		"final Patchwork check will come from the reports-list archive",
		"State saved after step",
		"Polling cycle 1 finished",
	)
	buf.Reset()
	ids := e.gh.startRuns("series_7", "sha-701", "Build")
	e.advance(3 * time.Minute)
	e.step()
	requireLogs(t, &buf,
		"Build run 1001, attempt 1: in_progress.",
		"posting pending check",
		"pending check github-robot-_Build posted successfully",
		"sending the pending report",
	)
	buf.Reset()
	e.gh.finishRun(ids[0], "success")
	e.advance(3 * time.Minute)
	e.step()
	requireLogs(t, &buf,
		"workflow Build completed with success",
		"waiting until",
		"for late workflows",
		"final report for github-robot: Build (success) sent",
	)
	buf.Reset()
	e.advance(11 * time.Minute)
	e.step()
	requireLogs(t, &buf,
		"testing and reporting are complete",
		"all patches and reports are complete",
		"moving to history with status done",
	)
	if e.p.Store.Find(7) != nil {
		t.Fatal("completed series is still queued")
	}
}

func TestLogsExplainCapacityAndHolds(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.cfg.Mode.Push = "off"
	e.step()
	requireLogs(t, &buf, "Not scheduling patches: push mode is \"off\"")
	s := e.p.Store.Find(7)
	s.Hold = true
	e.cfg.Mode.Push = "live"
	buf.Reset()
	e.p.schedule(context.Background())
	requireLogs(t, &buf, "not scheduling because it is on manual hold")
	s.Hold = false
	e.cfg.Limits.MaxQueuedJobs = 1
	e.gh.active = []gh.WorkflowRun{{ID: 1, Status: "queued"}}
	buf.Reset()
	e.p.schedule(context.Background())
	requireLogs(t, &buf,
		"1 queued jobs (limit 1)",
		"Stopped selecting patches: 1 jobs waiting for a runner on GitHub reach the "+
			"limit of 1.",
		"selected 0 patch slots",
	)
}

func TestLogsExplainFailureBackoff(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.pw.fail["series"] = 1
	e.step()
	requireLogs(t, &buf, "fetch series failed (attempt 1", "Next retry at", "HTTP 503")
	buf.Reset()
	e.advance(time.Minute)
	e.step()
	requireLogs(t, &buf, "waiting until", "to retry fetch series")
}

func TestLogsExplainIncompleteAndSkippedSeries(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.cfg.Mode.Push = "off"
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.pw.series[7].ReceivedAll = false
	e.pw.addSeries(8, 1, "ipsum", e.now)
	e.pw.series[8].Name = "[RFC] A proposal"
	e.step()
	requireLogs(t, &buf,
		"Series 7 (lorem: Series 7): received 1 of 1 patches; waiting for the rest",
		"Series 8 ([RFC] A proposal): skipping: RFC",
	)
	buf.Reset()
	e.advance(time.Minute)
	e.step()
	requireLogs(t, &buf, "waiting for missing patches; next completeness check at")
}

func TestLogsExplainDispatchAndTimeout(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.cfg.Workflows.Custom = true
	e.gh.files["leadtest-lint.yml"] = "on: [workflow_dispatch]\n"
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.step()
	requireLogs(t, &buf,
		"dispatching leadtest-lint.yml from main",
		"GitHub accepted the leadtest-lint.yml dispatch",
	)
	e.advance(2 * time.Hour)
	buf.Reset()
	e.step()
	requireLogs(t, &buf,
		"is being marked as a warning",
		"The dispatched workflow run was not found",
	)
}

func TestLogsExplainArchiveInitializationAndIngestion(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.step()
	requireLogs(t, &buf, "First scan of this archive", "not processing historical messages")
	e.reports.add(e.now, 1, "Build report",
		reportBody("lab", "success", 701), "<r@example.org>")
	e.p.reportsScanned = false
	buf.Reset()
	e.p.ingest(context.Background())
	requireLogs(t, &buf,
		"1 new after saved message 0",
		"Reading the archived report https://",
		"posting success check lab",
		"posted success check lab",
	)
}

func TestLogsNameTheExactReason(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.pw.addSeries(7, 2, "lorem", e.now)
	e.step()
	known, err := e.p.known()
	if err != nil {
		t.Fatal(err)
	}
	var n seen
	e.p.track(7, "", known, &n)
	e.p.track(0, "", known, &n)
	requireLogs(t, &buf, "Ignoring series ID 0 from Patchwork: not a valid ID.")
	if n.String() != "1 already in the queue (7)" {
		t.Errorf("summary = %q", n.String())
	}

	// A run of another branch, and a series waiting for its runs.
	buf.Reset()
	other := e.gh.startRuns("series_8", "sha-701", "Build")[0]
	e.gh.startRuns("series_7", "sha-701", "Build")
	e.advance(3 * time.Minute)
	e.step()
	requireLogs(t, &buf,
		"ignoring run "+strconv.FormatInt(other, 10)+
			" (Build): it ran on branch series_8, not series_7.",
		"Series with patches still being tested: 7 (2 of 2).",
	)

	// Recheck reasons for each run that is not rerun.
	buf.Reset()
	pt := e.series(7).Patches[0]
	pt.Runs = append(pt.Runs, &state.Run{Kind: state.RunPush, Workflow: "Lint",
		Status: "completed", Result: state.ResultSuccess, ID: 99})
	pt.Recheck = true
	e.p.doRecheck(context.Background(), e.series(7), pt)
	requireLogs(t, &buf,
		"not rerunning apply-robot; it is the result of applying the patch, "+
			"not a workflow run.",
		"not rerunning github-robot: Build; it has not finished yet (in_progress).",
		"not rerunning github-robot: Lint; it passed, and workflows.rerun_jobs only "+
			"reruns failures.",
	)

	e.series(7).Finish(state.SeriesDone, "", e.now)
	if err := e.p.Store.Archive(e.series(7)); err != nil {
		t.Fatal(err)
	}
	n = seen{}
	e.p.track(7, "", map[int]bool{7: true}, &n)
	if n.String() != "1 already in the history (7)" {
		t.Errorf("summary = %q", n.String())
	}
}

func TestLogsSummarizeRepeatedWork(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.Limits.MaxInflightPatches = 2
	e.pw.addSeries(300, 2, "lorem", e.now)
	e.pw.addSeries(301, 2, "ipsum", e.now)
	e.pw.addSeries(302, 1, "dolor", e.now)
	e.step()
	e.gh.startRuns("series_300", "sha-30001", "Build")
	e.advance(3 * time.Minute)
	e.step()

	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.advance(3 * time.Minute)
	e.step()
	requireLogs(t, &buf,
		"Ready to push: 3 patches of series 300 (1), 301 (1), 302 (1).",
		"Series with patches still being tested: 300 (2 of 2), 301 (2 of 2).",
		"0 of 1 workflows finished; waiting for Build,",
	)
	if strings.Contains(buf.String(), "attempt 1: in_progress") {
		t.Errorf("an unchanged run was logged again:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "found 1 matching push workflows") {
		t.Errorf("per-patch progress is still logged:\n%s", buf.String())
	}
}

func TestLogsSummarizeUnsentReports(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(303, 2, "sit", e.now)
	e.repo.failAt[303] = 0
	e.mail.fail = 100
	var buf bytes.Buffer
	e.p.Log = logx.New(&buf, false)
	e.step()
	requireLogs(t, &buf, "Series waiting only for reports to be sent: 303 (1).")
}
