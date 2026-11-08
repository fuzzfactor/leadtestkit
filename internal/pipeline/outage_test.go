// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"strings"
	"testing"
	"time"

	"leadtestkit/internal/state"
)

func outageEnv(t *testing.T) *env {
	e := newEnv(t, "")
	e.cfg.Report.ApplySuccess = false
	e.cfg.Limits.MaxInflightPatches = 1
	e.cfg.Alerts.To = []string{"ipsum@example.org", "dolor@example.org"}
	return e
}

func TestOutageWithoutRuns(t *testing.T) {
	e := outageEnv(t)
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.pw.addSeries(81, 1, "ipsum", e.now)
	e.step()
	if e.gh.setBranch != 1 {
		t.Fatalf("pushes = %d", e.gh.setBranch)
	}

	// Nothing runs on the repository: no warnings, but an alert.
	e.advance(61 * time.Minute)
	e.step()
	o := e.p.Store.Meta.Outage
	if o == nil || o.Kind != state.OutageRuns || !strings.Contains(o.Reason,
		"no workflow run started") {
		t.Fatalf("outage = %+v", o)
	}
	if len(e.mail.sent) != 0 {
		t.Errorf("reports were sent: %q", e.mail.subjects())
	}
	alerts := e.alerts.subjects()
	if len(alerts) != 1 || alerts[0] != "LeadTestKit: workflows do not run on ipsum/lorem" ||
		len(e.alerts.sent[0].Recipients) != 2 || len(e.p.Stats.Alerts) != 1 {
		t.Fatalf("alerts = %q", alerts)
	}
	pt := e.series(80).Patches[0]
	if pt.Status != state.PatchApplied || pt.Pushed != "" || len(ciRuns(pt)) != 0 ||
		e.gh.branches["series_80"] != "" || e.series(80).Branch != "" {
		t.Fatalf("patch = %+v, branch %q", pt, e.gh.branches["series_80"])
	}
	decision(t, pt.Decisions, "ci-unavailable", "")

	// No pushes until the hourly check, which pushes one patch.
	e.advance(30 * time.Minute)
	e.step()
	if e.gh.setBranch != 1 {
		t.Fatalf("pushed during the outage: %d", e.gh.setBranch)
	}
	e.advance(31 * time.Minute)
	e.step()
	if e.gh.setBranch != 2 || pt.Status != state.PatchPushed {
		t.Fatalf("no check push: %d pushes, patch %s", e.gh.setBranch, pt.Status)
	}
	e.advance(31 * time.Minute)
	e.step()
	if pt.Status != state.PatchApplied || len(e.alerts.sent) != 1 {
		t.Fatalf("a check without runs: patch %s, %d alerts", pt.Status,
			len(e.alerts.sent))
	}

	// A reminder once a day.
	e.reload()
	e.advance(24 * time.Hour)
	e.step()
	if alerts := e.alerts.subjects(); len(alerts) != 2 ||
		!strings.Contains(alerts[1], "still do not run") {
		t.Fatalf("alerts = %q", alerts)
	}

	// Workflows run again for the next check.
	e.advance(time.Hour)
	e.step()
	pt = e.series(80).Patches[0]
	if pt.Status != state.PatchPushed || e.gh.branches["series_80"] != "sha-8001" {
		t.Fatalf("patch = %+v", pt)
	}
	e.gh.startRuns("series_80", "sha-8001", "Build")
	e.advance(3 * time.Minute)
	e.step()
	if e.p.Store.Meta.Outage != nil {
		t.Fatalf("outage = %+v", e.p.Store.Meta.Outage)
	}
	if alerts := e.alerts.subjects(); len(alerts) != 3 ||
		!strings.Contains(alerts[2], "run again") {
		t.Fatalf("alerts = %q", alerts)
	}
	if r := pt.Run(state.RunPush, "Build"); r == nil || r.ID == 0 {
		t.Fatalf("runs = %+v", pt.Runs)
	}
}

func TestOutageNeedsAQuietRepository(t *testing.T) {
	e := outageEnv(t)
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.step()
	e.gh.startRuns("main", "sha-main", "Build")
	e.advance(61 * time.Minute)
	e.step()
	if e.p.Store.Meta.Outage != nil || len(e.alerts.sent) != 0 {
		t.Fatalf("outage = %+v", e.p.Store.Meta.Outage)
	}
	if r := ciRuns(e.series(80).Patches[0]); len(r) != 1 || r[0].Kind != state.RunMissing {
		t.Fatalf("runs = %+v", r)
	}
}

func TestOutageOfDispatches(t *testing.T) {
	e := outageEnv(t)
	e.cfg.Workflows.Custom = true
	e.gh.files["leadtest-lint.yml"] = "name: Lint\non: [workflow_dispatch]\n"
	e.gh.disabled = true
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.step()
	o := e.p.Store.Meta.Outage
	if o == nil || o.Kind != state.OutageDispatch ||
		o.Reason != "GitHub refused to dispatch leadtest-lint.yml: "+
			"Actions has been disabled for this repository" {
		t.Fatalf("outage = %+v", o)
	}
	pt := e.series(80).Patches[0]
	lint := runByFile(pt, "leadtest-lint.yml")
	// The push workflows still ran; the dispatch waits without a
	// deadline and without timing out.
	e.gh.finishRun(e.gh.startRuns("series_80", "sha-8001", "Build")[0], "success")
	for i := 0; i < 4; i++ {
		e.advance(24 * time.Hour)
		e.step()
	}
	if lint.Status != "dispatching" || lint.Result != "" || pt.Status != state.PatchPushed {
		t.Fatalf("lint = %+v", lint)
	}
	if n := len(e.gh.dispatches); n != 0 {
		t.Fatalf("dispatches = %d", n)
	}

	e.gh.disabled = false
	e.advance(time.Hour)
	e.step()
	if e.p.Store.Meta.Outage != nil || lint.Status != "requested" {
		t.Fatalf("outage = %+v, lint = %+v", e.p.Store.Meta.Outage, lint)
	}
	if alerts := e.alerts.subjects(); len(alerts) < 2 ||
		!strings.Contains(alerts[len(alerts)-1], "run again") {
		t.Fatalf("alerts = %q", alerts)
	}
}

func TestOutageAlertRetried(t *testing.T) {
	e := outageEnv(t)
	e.alerts.fail = 1
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.step()
	e.advance(61 * time.Minute)
	e.step()
	if o := e.p.Store.Meta.Outage; o == nil || o.Alerted != "" || len(e.alerts.sent) != 0 {
		t.Fatalf("outage = %+v", o)
	}
	e.advance(3 * time.Minute)
	e.step()
	if o := e.p.Store.Meta.Outage; o.Alerted == "" || len(e.alerts.sent) != 1 {
		t.Fatalf("outage = %+v, %d alerts", o, len(e.alerts.sent))
	}
}
