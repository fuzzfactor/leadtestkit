// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"leadtestkit/internal/logx"
	"leadtestkit/internal/state"
)

const firstBotRun = "https://github.com/ipsum/bot/actions/runs/1/attempts/1"
const nextBotRun = "https://github.com/ipsum/bot/actions/runs/2/attempts/1"

func decision(t *testing.T, list []state.Decision, action, runURL string) state.Decision {
	t.Helper()
	for _, d := range list {
		if d.Action == action && d.RunURL == runURL {
			if state.ParseTime(d.Time).IsZero() || d.Detail == "" {
				t.Fatalf("incomplete decision: %+v", d)
			}
			return d
		}
	}
	t.Fatalf("missing %s decision linked to %s: %+v", action, runURL, list)
	return state.Decision{}
}

func TestDecisionLinksSurviveTheLifecycle(t *testing.T) {
	e := newEnv(t, "")
	e.p.RunURL = firstBotRun
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.step()
	s := e.series(7)
	pt := s.Patches[0]
	for _, action := range []string{"discovered", "queued", "base-selected"} {
		decision(t, s.Decisions, action, firstBotRun)
	}
	for _, action := range []string{"discovered", "applied", "pushed", "report-sent"} {
		decision(t, pt.Decisions, action, firstBotRun)
	}
	e.reload()
	e.p.RunURL = nextBotRun
	ids := e.gh.startRuns("series_7", "sha-701", "Build")
	e.advance(3 * time.Minute)
	e.step()
	s = e.series(7)
	pt = s.Patches[0]
	decision(t, pt.Decisions, "workflow-updated", nextBotRun)
	decision(t, pt.Decisions, "check-posted", nextBotRun)
	decision(t, pt.Decisions, "report-sent", nextBotRun)
	decision(t, pt.Decisions, "pushed", firstBotRun)
	e.gh.finishRun(ids[0], "success")
	e.advance(3 * time.Minute)
	e.step()
	e.advance(11 * time.Minute)
	e.step()
	e.reload()
	s = e.series(7)
	decision(t, s.Decisions, "done", nextBotRun)
	decision(t, s.Patches[0].Decisions, "done", nextBotRun)
	decision(t, s.Patches[0].Decisions, "pushed", firstBotRun)

	thirdBotRun := "https://github.com/ipsum/bot/actions/runs/3/attempts/2"
	e.p.RunURL = thirdBotRun
	e.cfg.Workflows.RerunJobs = "all"
	s.Patches[0].Recheck = true
	e.p.doRecheck(context.Background(), s, s.Patches[0])
	if e.p.Store.Find(7) == nil {
		t.Fatal("recheck did not reopen the series")
	}
	if err := e.p.Store.Save(); err != nil {
		t.Fatal(err)
	}
	e.reload()
	s = e.series(7)
	decision(t, s.Patches[0].Decisions, "pushed", firstBotRun)
	decision(t, s.Patches[0].Decisions, "rerun", thirdBotRun)
}

func TestNoDecisionForRepeatedPolling(t *testing.T) {
	e := newEnv(t, "")
	e.p.RunURL = firstBotRun
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.step()
	s := e.series(7)
	pt := s.Patches[0]
	n, m := len(s.Decisions), len(pt.Decisions)
	e.step()
	e.step()
	if len(s.Decisions) != n || len(pt.Decisions) != m {
		t.Fatalf("polling added decisions: series %d to %d; patch %d to %d",
			n, len(s.Decisions), m, len(pt.Decisions))
	}
}

func TestFailedPushAndRetryHaveSeparateLinks(t *testing.T) {
	e := newEnv(t, "")
	e.p.RunURL = firstBotRun
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.gh.fail["set-branch"] = 1
	e.step()
	pt := e.series(7).Patches[0]
	decision(t, pt.Decisions, "push-failed", firstBotRun)
	e.p.RunURL = nextBotRun
	e.advance(6 * time.Minute)
	e.step()
	e.reload()
	pt = e.series(7).Patches[0]
	decision(t, pt.Decisions, "push-failed", firstBotRun)
	decision(t, pt.Decisions, "pushed", nextBotRun)
}

func TestSkippedSeriesHasADecisionLink(t *testing.T) {
	e := newEnv(t, "")
	e.p.RunURL = firstBotRun
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.pw.series[7].Name = "[RFC] An idea"
	e.step()
	e.reload()
	d := decision(t, e.series(7).Decisions, "skipped", firstBotRun)
	if d.Detail != "RFC" {
		t.Fatalf("skip reason was not recorded: %+v", d)
	}
}

func TestArchivedCheckDecisionIsSaved(t *testing.T) {
	e := newEnv(t, "")
	e.p.RunURL = firstBotRun
	e.pw.addSeries(7, 1, "lorem", e.now)
	e.step()
	s := e.series(7)
	s.Finish(state.SeriesDone, "", e.now)
	if err := e.p.Store.Archive(s); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Store.Save(); err != nil {
		t.Fatal(err)
	}
	e.reload()
	e.p.RunURL = nextBotRun
	u := e.reports.add(e.now, 10, "Build report",
		reportBody("lab", "success", 701), "<report@example.org>")
	if err := e.p.ingestMessage(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if err := e.p.Store.Save(); err != nil {
		t.Fatal(err)
	}
	e.reload()
	s = e.series(7)
	d := decision(t, s.Patches[0].Decisions, "check-posted", nextBotRun)
	if !strings.Contains(d.Detail, u) {
		t.Fatalf("archived check is missing its report URL: %+v", d)
	}
	if e.p.Store.Find(7) != nil {
		t.Fatal("recording a check reopened the series")
	}
}

func TestLocalDecisionsAndRepeatedActions(t *testing.T) {
	e := newEnv(t, "")
	pt := &state.Patch{ID: 701}
	e.p.notePatch(pt, "pushed", "Branch %s.", "series_7")
	e.advance(time.Minute)
	e.p.notePatch(pt, "pushed", "Branch %s.", "series_7")
	if len(pt.Decisions) != 2 || pt.Decisions[0].RunURL != "" {
		t.Fatalf("local decisions: %+v", pt.Decisions)
	}
	if pt.Decisions[0].Time == pt.Decisions[1].Time {
		t.Fatal("repeated actions lost their individual timestamps")
	}
	e.p.RunURL = firstBotRun
	e.p.notePatch(pt, "pushed", "Branch %s.", "series_7")
	if len(pt.Decisions) != 3 {
		t.Fatalf("a new run was not recorded: %+v", pt.Decisions)
	}
}

func TestDecisionDetailsAreRedacted(t *testing.T) {
	e := newEnv(t, "")
	logx.AddSecret("decision-test-secret")
	pt := &state.Patch{ID: 701}
	e.p.notePatch(pt, "push-failed", "Failed: %s", "decision-test-secret")
	if pt.Decisions[0].Detail != "Failed: [REDACTED]" {
		t.Fatalf("decision leaked a secret: %+v", pt.Decisions)
	}
}
