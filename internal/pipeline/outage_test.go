// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"leadtestkit/internal/ghapp"
	"leadtestkit/internal/httpx"
	"leadtestkit/internal/mail"
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
	o := e.p.outage(state.OutageRuns)
	if o == nil || !strings.Contains(o.Reason,
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
		alerts[1] != "LeadTestKit: reminder: workflows do not run on ipsum/lorem" {
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
	if len(e.p.Store.Meta.Outages) != 0 {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
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
	if len(e.p.Store.Meta.Outages) != 0 || len(e.alerts.sent) != 0 {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
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
	o := e.p.outage(state.OutageDispatch)
	if o == nil ||
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
	if len(e.p.Store.Meta.Outages) != 0 || lint.Status != "requested" {
		t.Fatalf("outages = %+v, lint = %+v", e.p.Store.Meta.Outages, lint)
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
	if o := e.p.outage(state.OutageRuns); o == nil || o.Alerted != "" ||
		len(e.alerts.sent) != 0 {
		t.Fatalf("outage = %+v", o)
	}
	e.advance(3 * time.Minute)
	e.step()
	if o := e.p.outage(state.OutageRuns); o.Alerted == "" || len(e.alerts.sent) != 1 {
		t.Fatalf("outage = %+v, %d alerts", o, len(e.alerts.sent))
	}
}

func TestOutageOfTheGitHubApp(t *testing.T) {
	e := outageEnv(t)
	e.gh.authErr = &ghapp.AuthError{Err: errors.New("the GitHub App lorem is not " +
		"installed on ipsum/lorem")}
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.step()
	o := e.p.outage(state.OutageGitHub)
	if o == nil || !strings.Contains(o.Reason, "not installed") {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
	if alerts := e.alerts.subjects(); len(alerts) != 1 || alerts[0] !=
		"LeadTestKit: GitHub refuses the GitHub App of the bot" {
		t.Fatalf("alerts = %q", alerts)
	}
	// Failures wait for the App instead of their retry deadline.
	for i := 0; i < 3; i++ {
		e.advance(24 * time.Hour)
		e.step()
	}
	s := e.series(80)
	if s.Status != state.SeriesTesting || s.Patches[0].Status != state.PatchApplied ||
		e.gh.setBranch != 0 {
		t.Fatalf("series = %+v, patch %+v", s, s.Patches[0])
	}
	if f := s.Patches[0].Failure; f == nil ||
		state.ParseTime(f.Next).Sub(e.now) > outageProbeInterval {
		t.Fatalf("failure = %+v", f)
	}

	e.gh.authErr = nil
	e.step()
	if len(e.p.Store.Meta.Outages) != 0 || e.gh.setBranch != 1 {
		t.Fatalf("outages = %+v, %d pushes", e.p.Store.Meta.Outages, e.gh.setBranch)
	}
	if alerts := e.alerts.subjects(); !strings.Contains(alerts[len(alerts)-1],
		"accepts the GitHub App of the bot again") {
		t.Fatalf("alerts = %q", alerts)
	}
}

func TestOutageOfEmail(t *testing.T) {
	e := outageEnv(t)
	e.cfg.Report.ApplySuccess = true
	down := &mail.ServerError{Err: errors.New("smtp dial smtp.example.org:587: " +
		"connection refused")}
	e.mail.err, e.alerts.err = down, down
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.step()
	// Brief failures are no outage.
	if e.p.outage(state.OutageEmail) != nil || len(e.p.Stats.Alerts) != 0 {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
	for i := 0; i < 5; i++ {
		e.advance(20 * time.Minute)
		e.step()
	}
	o := e.p.outage(state.OutageEmail)
	if o == nil || len(e.p.Stats.Alerts) != 1 ||
		!strings.Contains(e.p.Stats.Alerts[0], "connection refused") {
		t.Fatalf("outage = %+v, alerts %q", o, e.p.Stats.Alerts)
	}
	// The report waits past the retry deadline.
	for i := 0; i < 3; i++ {
		e.advance(24 * time.Hour)
		e.reload()
		e.step()
	}
	r := e.series(80).Patches[0].Runs[0]
	if r.Final.Email != "" || r.Failure == nil {
		t.Fatalf("run = %+v", r)
	}
	if len(e.p.Stats.Alerts) != 1 {
		t.Errorf("no daily reminder in the failing run: %q", e.p.Stats.Alerts)
	}

	e.mail.err, e.alerts.err = nil, nil
	e.step()
	if r.Final.Email != state.StepSent || e.p.outage(state.OutageEmail) != nil {
		t.Fatalf("run = %+v, outages %+v", r, e.p.Store.Meta.Outages)
	}
	// Alerts that waited for email are sent too.
	if alerts := strings.Join(e.alerts.subjects(), "\n"); !strings.Contains(alerts,
		"emails can be sent again") || !strings.Contains(alerts, "workflows do not run") {
		t.Fatalf("alerts = %q", alerts)
	}
}

func TestOutageOfTheArchive(t *testing.T) {
	e := outageEnv(t)
	e.step()
	e.reports.monthFail = 1000
	for i := 0; i < 5; i++ {
		e.advance(7 * time.Hour)
		e.reload()
		e.step()
	}
	o := e.p.outage(state.OutageArchive(reportsCursor))
	if o == nil || o.Since != "2026-03-10T19:00:00Z" {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
	if alerts := e.alerts.subjects(); len(alerts) != 1 ||
		alerts[0] != "LeadTestKit: the reports archive cannot be read" {
		t.Fatalf("alerts = %q", alerts)
	}
	e.reports.monthFail = 0
	e.reload()
	e.step()
	if len(e.p.Store.Meta.Outages) != 0 || e.p.Store.Cursor(reportsCursor).Failing != "" {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
}

func TestShortArchiveFailuresAreNoOutage(t *testing.T) {
	e := outageEnv(t)
	e.step()
	e.reports.monthFail = 1
	e.reload()
	e.step()
	if e.p.Store.Cursor(reportsCursor).Failing == "" {
		t.Fatal("the failure was not recorded")
	}
	e.advance(time.Hour)
	e.reload()
	e.step()
	if len(e.p.Store.Meta.Outages) != 0 || e.p.Store.Cursor(reportsCursor).Failing != "" {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
}

// The alert for one outage can end the email outage that kept it from
// being sent, while the outages are being checked.
func TestAlertEndingTheEmailOutage(t *testing.T) {
	e := outageEnv(t)
	since := state.FormatTime(e.now)
	e.p.Store.Meta.Outages = map[string]*state.Outage{
		state.OutageDispatch: {Since: since, Reason: "lorem",
			NextProbe: state.FormatTime(e.now.Add(time.Hour))},
		state.OutageEmail: {Since: since, Reason: "ipsum", Alerted: since},
	}
	e.step()
	if e.p.outage(state.OutageEmail) != nil || e.p.outage(state.OutageDispatch) == nil {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
	if alerts := strings.Join(e.alerts.subjects(), "\n"); !strings.Contains(alerts,
		"workflows do not run") || !strings.Contains(alerts, "emails can be sent again") {
		t.Fatalf("alerts = %q", alerts)
	}
}

func TestOutageEndRestartsDeadlines(t *testing.T) {
	e := outageEnv(t)
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.step()
	s := e.series(80)
	pt := s.Patches[0]
	old := state.FormatTime(e.now.Add(-10 * 24 * time.Hour))
	pt.Failure = &state.Failure{Op: "push", Attempts: 9, First: old, Next: old,
		Outage: state.OutageGitHub}
	other := &state.Failure{Op: "fetch series", Attempts: 3, First: old, Next: old}
	s.Failure = other
	e.p.startOutage(state.OutageGitHub, "lorem")
	e.advance(time.Hour)
	e.p.endOutage(state.OutageGitHub, "ipsum")
	now := state.FormatTime(e.now)
	if f := pt.Failure; f.First != now || f.Next != now || f.Attempts != 0 ||
		f.Outage != "" {
		t.Fatalf("failure = %+v", f)
	}
	// Failures that did not wait for the outage keep their deadline.
	if *s.Failure != (state.Failure{Op: "fetch series", Attempts: 3, First: old,
		Next: old}) {
		t.Fatalf("other failure = %+v", s.Failure)
	}

	// An ordinary error after waiting does not count the waiting.
	pt.Failure.Outage = state.OutageEmail
	pt.Failure.First = old
	if e.p.recordFailure(&pt.Failure, "push", errTransient, time.Hour) {
		t.Fatalf("gave up after the outage: %+v", pt.Failure)
	}
}

func TestOutagesOfUnusedServicesAreForgotten(t *testing.T) {
	e := outageEnv(t)
	since := state.FormatTime(e.now)
	e.p.Store.Meta.Outages = map[string]*state.Outage{
		state.OutageEmail:                  {Since: since, Reason: "lorem", Alerted: since},
		state.OutageArchive(reportsCursor): {Since: since, Reason: "ipsum", Alerted: since},
	}
	e.p.Store.Cursor(reportsCursor).Failing = since
	e.cfg.Mode.Email = "off"
	e.cfg.Archive.Ingest = false
	e.step()
	if len(e.p.Store.Meta.Outages) != 0 || e.p.Store.Cursor(reportsCursor).Failing != "" {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
}

func TestOneOutageOfWorkflows(t *testing.T) {
	e := outageEnv(t)
	e.cfg.Workflows.Custom = true
	e.gh.files["leadtest-lint.yml"] = "name: Lint\non: [workflow_dispatch]\n"
	e.p.startOutage(state.OutageRuns, "lorem")
	e.gh.disabled = true
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.advance(time.Hour)
	e.step()
	if len(e.gh.dispatches) != 0 || e.p.outage(state.OutageDispatch) != nil ||
		len(e.alerts.sent) != 1 {
		t.Fatalf("outages = %+v, %d alerts", e.p.Store.Meta.Outages, len(e.alerts.sent))
	}
	e.gh.disabled = false
	e.advance(time.Hour)
	e.step()
	if len(e.p.Store.Meta.Outages) != 0 || len(e.alerts.sent) != 2 {
		t.Fatalf("outages = %+v, alerts %q", e.p.Store.Meta.Outages, e.alerts.subjects())
	}
}

func TestEmailOutageWithoutAlertsKeepsRunsGreen(t *testing.T) {
	e := outageEnv(t)
	e.cfg.Alerts.To = nil
	e.p.startOutage(state.OutageEmail, "lorem")
	if len(e.p.Stats.Alerts) != 0 {
		t.Fatalf("alerts = %q", e.p.Stats.Alerts)
	}
}

func TestDispatchesWaitDuringOutageOfRuns(t *testing.T) {
	e := outageEnv(t)
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.step()
	e.gh.finishRun(e.gh.startRuns("series_80", "sha-8001", "Build")[0], "success")
	pt := e.series(80).Patches[0]
	lint := &state.Run{Kind: state.RunDispatch, File: "leadtest-lint.yml",
		Status: "dispatching", CorrelationID: "80-8001-leadtest-lint-1"}
	pt.Runs = append(pt.Runs, lint)
	e.p.startOutage(state.OutageRuns, "lorem")
	e.gh.disabled = true
	e.advance(e.cfg.Limits.RunTimeout.D() + time.Hour)
	e.step()
	if lint.Result != "" || lint.Status != "dispatching" || pt.Status != state.PatchPushed ||
		e.p.outage(state.OutageDispatch) != nil {
		t.Fatalf("lint = %+v", lint)
	}
}

// Without reports to send, failing alerts tell that email cannot be sent.
func TestFailingAlertsStartAnEmailOutage(t *testing.T) {
	e := outageEnv(t)
	e.alerts.err = &mail.ServerError{Err: errors.New("smtp dial: connection refused")}
	e.p.startOutage(state.OutageGitHub, "lorem")
	e.gh.authErr = &ghapp.AuthError{Err: errors.New("lorem")}
	e.step()
	if e.p.outage(state.OutageEmail) != nil {
		t.Fatal("an email outage at the first failed alert")
	}
	e.advance(61 * time.Minute)
	e.reload()
	e.step()
	if e.p.outage(state.OutageEmail) == nil || len(e.p.Stats.Alerts) != 1 {
		t.Fatalf("outages = %+v, alerts %q", e.p.Store.Meta.Outages, e.p.Stats.Alerts)
	}
}

// An outage that starts backdated, like one of an archive, does not
// make its first failed alert an email outage.
func TestFirstFailedAlertOfABackdatedOutage(t *testing.T) {
	e := outageEnv(t)
	e.alerts.err = &mail.ServerError{Err: errors.New("smtp dial: connection refused")}
	e.p.startOutageAt(state.OutageArchive(reportsCursor), "lorem",
		e.now.Add(-25*time.Hour))
	if e.p.outage(state.OutageEmail) != nil {
		t.Fatal("an email outage at the first failed alert")
	}
	e.advance(61 * time.Minute)
	e.p.outageCycle(context.Background())
	if e.p.outage(state.OutageEmail) == nil {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
}

// Server errors before an email outage starts are not waiting for it:
// ordinary errors in between keep their deadline.
func TestBriefServerErrorsKeepTheDeadline(t *testing.T) {
	e := outageEnv(t)
	var f *state.Failure
	server := &mail.ServerError{Err: errors.New("smtp dial: connection refused")}
	e.p.recordFailure(&f, "report email", server, 2*time.Hour)
	if f.Outage != "" || e.p.outage(state.OutageEmail) != nil {
		t.Fatalf("failure = %+v", f)
	}
	first := f.First
	e.advance(30 * time.Minute)
	e.p.recordFailure(&f, "report email", errTransient, 2*time.Hour)
	if f.First != first {
		t.Fatalf("the deadline restarted: %+v", f)
	}
	e.advance(2 * time.Hour)
	if !e.p.recordFailure(&f, "report email", errTransient, 2*time.Hour) {
		t.Fatalf("never gave up: %+v", f)
	}
}

func TestOutageOfPatchwork(t *testing.T) {
	e := outageEnv(t)
	start := state.FormatTime(e.now)
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.pw.errs["series"] = errTransient
	e.step()
	s := e.series(80)
	if s.Failure == nil || s.Failure.Cause != state.OutagePatchwork ||
		s.Failure.Outage != "" || e.failingSince(state.ServicePatchwork) != start {
		t.Fatalf("failure = %+v, failing since %q", s.Failure,
			e.failingSince(state.ServicePatchwork))
	}

	// Two hours without answers are an outage since the first failure.
	e.pw.errs["*"] = errTransient
	e.advance(time.Hour)
	e.reload()
	e.step()
	if e.p.outage(state.OutagePatchwork) != nil {
		t.Fatal("outage after an hour")
	}
	e.advance(61 * time.Minute)
	e.reload()
	e.step()
	o := e.p.outage(state.OutagePatchwork)
	if o == nil || o.Since != start || o.Checks {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
	if alerts := e.alerts.subjects(); len(alerts) != 1 ||
		alerts[0] != "LeadTestKit: Patchwork is unavailable" {
		t.Fatalf("alerts = %q", alerts)
	}
	s = e.series(80)
	if s.Failure.Outage != state.OutagePatchwork {
		t.Fatalf("the earlier failure does not wait for the outage: %+v", s.Failure)
	}

	// The series waits past its retry deadline, without being asked for.
	calls := e.pw.calls["series"]
	for i := 0; i < 3; i++ {
		e.advance(24 * time.Hour)
		e.reload()
		e.step()
	}
	if s = e.series(80); s.Status != state.SeriesNew || e.pw.calls["series"] != calls {
		t.Fatalf("series = %+v, %d requests", s, e.pw.calls["series"]-calls)
	}

	delete(e.pw.errs, "*")
	delete(e.pw.errs, "series")
	e.reload()
	e.step()
	if s = e.series(80); s.Status != state.SeriesTesting ||
		e.p.outage(state.OutagePatchwork) != nil ||
		e.failingSince(state.ServicePatchwork) != "" {
		t.Fatalf("series = %+v, outages %+v", s, e.p.Store.Meta.Outages)
	}
	if alerts := e.alerts.subjects(); alerts[len(alerts)-1] !=
		"LeadTestKit: Patchwork is available again" {
		t.Fatalf("alerts = %q", alerts)
	}
}

func TestPatchworkRefusingTheToken(t *testing.T) {
	e := outageEnv(t)
	e.pw.errs["*"] = &httpx.StatusError{Code: 401,
		URL: "https://pw.example.org/api/1.2/events/", Body: "Invalid token."}
	e.step()
	o := e.p.outage(state.OutagePatchwork)
	if o == nil || !strings.Contains(o.Reason, "Invalid token") {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
}

func TestPatchworkRefusingChecks(t *testing.T) {
	e := outageEnv(t)
	e.cfg.Mode.Push = "off"
	e.step()
	e.reload()
	e.pw.addSeries(90, 1, "lorem", e.now)
	u := e.reports.add(e.now, 1, "|FAILURE| pw9001 x",
		reportBody("lab-robot: Perf test", "FAILURE", 9001), "<b@lab>")
	e.pw.errs["create-check"] = &httpx.StatusError{Code: 403,
		URL:  "https://pw.example.org/api/1.2/patches/9001/checks/",
		Body: "You do not have permission to perform this action."}
	e.step()
	o := e.p.outage(state.OutagePatchwork)
	if o == nil || !o.Checks {
		t.Fatalf("outages = %+v", e.p.Store.Meta.Outages)
	}
	if alerts := e.alerts.subjects(); len(alerts) != 1 ||
		alerts[0] != "LeadTestKit: Patchwork refuses the checks of the bot" {
		t.Fatalf("alerts = %q", alerts)
	}
	f := e.p.Store.Cursor(reportsCursor).Retry[u]
	if f == nil || f.Outage != state.OutagePatchwork {
		t.Fatalf("report failure = %+v", f)
	}

	// Patchwork answering other requests does not end it.
	e.advance(time.Hour)
	e.reload()
	e.step()
	if again := e.p.outage(state.OutagePatchwork); again == nil ||
		again.Since != o.Since || len(e.alerts.sent) != 1 {
		t.Fatalf("reads ended the outage: %+v, alerts %q", again, e.alerts.subjects())
	}

	delete(e.pw.errs, "create-check")
	e.advance(time.Hour)
	e.reload()
	e.step()
	if len(e.pw.posted) != 1 || e.p.outage(state.OutagePatchwork) != nil ||
		len(e.p.Store.Cursor(reportsCursor).Retry) != 0 {
		t.Fatalf("posted = %+v, outages %+v", e.pw.posted, e.p.Store.Meta.Outages)
	}
	if alerts := e.alerts.subjects(); alerts[len(alerts)-1] !=
		"LeadTestKit: Patchwork accepts the checks of the bot again" {
		t.Fatalf("alerts = %q", alerts)
	}
}

// Short failures are recorded too, and outages mark their period.
func TestAvailabilityRecorded(t *testing.T) {
	e := outageEnv(t)
	start := state.FormatTime(e.now)
	e.pw.errs["*"] = errTransient
	e.step()
	e.advance(10 * time.Minute)
	delete(e.pw.errs, "*")
	e.reload()
	e.step()
	a := e.p.Store.Availability
	if len(a.Periods) != 1 || a.Periods[0].Service != state.ServicePatchwork ||
		a.Periods[0].Since != start || a.Periods[0].Until != state.FormatTime(e.now) ||
		a.Periods[0].Outage || len(a.Failing) != 0 {
		t.Fatalf("availability = %+v", a)
	}

	// Email server errors, and the outage they become.
	down := &mail.ServerError{Err: errors.New("connection refused")}
	e.mail.err, e.alerts.err = down, down
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.cfg.Report.ApplySuccess = true
	for i := 0; i < 5; i++ {
		e.advance(20 * time.Minute)
		e.reload()
		e.step()
	}
	if per := e.p.Store.Availability.Failing[state.ServiceEmail]; per == nil || !per.Outage {
		t.Fatalf("email = %+v, outages %+v", per, e.p.Store.Meta.Outages)
	}
	e.mail.err, e.alerts.err = nil, nil
	e.reload()
	e.step()
	a = e.p.Store.Availability
	if last := a.Periods[len(a.Periods)-1]; last.Service != state.ServiceEmail ||
		!last.Outage || a.Failing[state.ServiceEmail] != nil {
		t.Fatalf("availability = %+v", a)
	}
}

func TestGitHubAvailability(t *testing.T) {
	e := outageEnv(t)
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.gh.fail["set-branch"] = 10
	e.step()
	per := e.p.Store.Availability.Failing[state.ServiceGitHub]
	if per == nil || !strings.Contains(per.Reason, "503") {
		t.Fatalf("availability = %+v", e.p.Store.Availability)
	}
	e.gh.fail["set-branch"] = 0
	e.advance(time.Hour)
	e.reload()
	e.step()
	if e.p.Store.Availability.Failing[state.ServiceGitHub] != nil {
		t.Fatalf("availability = %+v", e.p.Store.Availability)
	}
}
