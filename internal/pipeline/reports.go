// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"leadtestkit/internal/gh"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/report"
	"leadtestkit/internal/state"
)

const logFetchAttempts = 3

func (p *Pipeline) report(ctx context.Context) {
	for _, s := range p.Store.Queue {
		if s.Status != state.SeriesTesting {
			continue
		}
		for _, pt := range sortedPatches(s) {
			for _, r := range pt.Runs {
				if ctx.Err() != nil {
					return
				}
				p.dropPendingRetries(pt, r)
				if !r.Failure.Due(p.now()) {
					p.infof("%s: delaying %s report until the %s retry at %s.",
						patchName(pt), p.label(r),
						r.Failure.Op, r.Failure.Next)
					continue
				}
				p.reportPending(ctx, s, pt, r)
				p.reportFinal(ctx, s, pt, r)
			}
		}
	}
}

func (p *Pipeline) label(r *state.Run) string {
	switch {
	case r.Kind == state.RunApply:
		return p.Cfg.Report.ApplyLabel
	case r.Workflow == "":
		return p.Cfg.Report.Label
	}
	return p.Cfg.Report.Label + ": " + r.Workflow
}

func resultWord(result string) string {
	switch result {
	case state.ResultSuccess:
		return "passed"
	case state.ResultFail:
		return "failed"
	}
	return result
}

func (p *Pipeline) description(r *state.Run, result string) string {
	prefix := p.Cfg.Report.BuildPrefix
	if r.Kind == state.RunApply {
		prefix = "apply robot"
	}
	return prefix + ": " + resultWord(result)
}

// dropPendingRetries gives up the pending reports of a completed run
// that wait for a retry, so that the final report does not wait for
// them; the final check replaces the pending one.
func (p *Pipeline) dropPendingRetries(pt *state.Patch, r *state.Run) {
	if r.Status != "completed" {
		return
	}
	if f := r.CheckFailure; f != nil {
		p.notePatch(pt, "check-failed", "Gave up the pending check for %s: the run "+
			"completed. Last error: %s", p.label(r), f.LastError)
		p.infof("%s: giving up the pending check for %s, which completed; the final "+
			"check replaces it. Last error: %s", patchName(pt), p.label(r), f.LastError)
		r.Pending.Check = state.StepFailed
		r.CheckFailure = nil
	}
	if f := r.Failure; f != nil && f.Op == "pending email" {
		p.notePatch(pt, "report-failed", "Gave up the pending email for %s: the run "+
			"completed. Last error: %s", p.label(r), f.LastError)
		p.infof("%s: giving up the pending email for %s, which completed; sending the "+
			"final report instead. Last error: %s", patchName(pt), p.label(r),
			f.LastError)
		r.Pending.Email = state.StepFailed
		r.Failure = nil
	}
}

func (p *Pipeline) reportPending(ctx context.Context, s *state.Series,
	pt *state.Patch, r *state.Run) {
	if r.Kind != state.RunPush && r.Kind != state.RunDispatch {
		return
	}
	if r.ID == 0 || r.Status == "completed" || r.Status == "requested" {
		return
	}
	p.pendingCheck(ctx, s, pt, r)
	p.pendingEmail(ctx, s, pt, r)
}

// pendingCheck posts the pending check of a run.  Its failures only
// delay its own retries.
func (p *Pipeline) pendingCheck(ctx context.Context, s *state.Series,
	pt *state.Patch, r *state.Run) {
	if r.Pending.Check != "" {
		return
	}
	if !p.Cfg.Report.PendingCheck() {
		p.notePatch(pt, "check-skipped", "Pending checks disabled for %s.", p.label(r))
		p.infof("%s: pending checks disabled; not posting one for %s.",
			patchName(pt), p.label(r))
		r.Pending.Check = state.StepSkipped
		return
	}
	if !r.CheckFailure.Due(p.now()) {
		p.infof("%s: delaying the pending check for %s until the retry at %s.",
			patchName(pt), p.label(r), r.CheckFailure.Next)
		return
	}
	target := r.URL
	if target == "" {
		target = p.branchURL(s.Branch)
	}
	chk := pw.NewCheck{
		State:       state.ResultPending,
		TargetURL:   target,
		Context:     report.Context(p.label(r)),
		Description: p.description(r, state.ResultPending),
	}
	if p.Cfg.Mode.PostChecks() && p.checksSkipped("posting pending checks") {
		return
	}
	if !p.Cfg.Mode.PostChecks() {
		p.infof("%s: would post pending check %s, checks mode is %s, linking to %s",
			patchName(pt), chk.Context, p.Cfg.Mode.Checks, chk.TargetURL)
		r.Pending.Check = state.StepLogged
		p.notePatch(pt, "check-logged", "Would post pending check %s linking to %s",
			chk.Context, chk.TargetURL)
		return
	}
	p.infof("%s: posting pending check %s linking to %s",
		patchName(pt), chk.Context, chk.TargetURL)
	if err := p.pw().CreateCheck(ctx, pt.ID, chk); err != nil {
		p.warnf("%s: pending check %s failed: %v", patchName(pt), chk.Context, err)
		if p.fail(&r.CheckFailure, "pending check", err) {
			r.Pending.Check = state.StepFailed
			r.CheckFailure = nil
		}
		p.notePatch(pt, "check-failed", "Pending check %s: %v", chk.Context, err)
		return
	}
	r.CheckFailure = nil
	r.Pending.Check = state.StepPosted
	p.Stats.Checks++
	p.notePatch(pt, "check-posted", "Pending check %s linking to %s.",
		chk.Context, chk.TargetURL)
	p.infof("%s: pending check %s posted successfully.", patchName(pt), chk.Context)
}

func (p *Pipeline) pendingEmail(ctx context.Context, s *state.Series,
	pt *state.Patch, r *state.Run) {
	if r.Pending.Email != "" {
		return
	}
	if !p.Cfg.Report.PendingEmail() {
		p.notePatch(pt, "report-skipped", "Pending emails disabled for %s.", p.label(r))
		p.infof("%s: pending emails disabled; not sending one for %s.",
			patchName(pt), p.label(r))
		r.Pending.Email = state.StepSkipped
		return
	}
	id, err := p.sendReport(ctx, s, pt, r, state.ResultPending, "pending", "")
	if err != nil {
		p.notePatch(pt, "report-failed", "Pending email for %s: %v", p.label(r), err)
		p.warnf("%s: pending email for %s failed: %v", patchName(pt), p.label(r), err)
		if p.fail(&r.Failure, "pending email", err) {
			r.Pending.Email = state.StepFailed
			r.Failure = nil
		}
		return
	}
	r.Failure = nil
	r.Pending.MessageID = id
	r.Pending.Email = emailStep(p.Cfg.Mode.EmailOff())
}

func emailStep(off bool) string {
	if off {
		return state.StepSkipped
	}
	return state.StepSent
}

func (p *Pipeline) reportFinal(ctx context.Context, s *state.Series,
	pt *state.Patch, r *state.Run) {
	if r.Status != "completed" || r.Result == "" || r.Final.Email != "" {
		return
	}
	extra := r.Details
	if r.Result != state.ResultSuccess && r.ID > 0 &&
		(r.Kind == state.RunPush || r.Kind == state.RunDispatch) {
		logs, err := p.failureLogs(ctx, r.ID)
		if err != nil {
			attempts := 0
			if r.Failure != nil && r.Failure.Op == "logs" {
				attempts = r.Failure.Attempts
			}
			if attempts+1 < logFetchAttempts {
				p.fail(&r.Failure, "logs", err)
				p.notePatch(pt, "report-delayed",
					"%s: cannot fetch logs; retry at %s: %v",
					p.label(r), r.Failure.Next, err)
				p.warnf("%s: cannot fetch logs for run %d; delaying its report: %v",
					patchName(pt), r.ID, err)
				return
			}
			logs = "Build logs are not available: " + err.Error()
			p.notePatch(pt, "report-without-logs", "%s: no logs after %d attempts: %v",
				p.label(r), attempts+1, err)
			p.warnf("%s: sending without build logs after %d failed attempts: %v",
				patchName(pt), attempts+1, err)
		}
		extra = joinNonEmpty(extra, logs)
	}
	id, err := p.sendReport(ctx, s, pt, r, r.Result, "final", extra)
	if err != nil {
		p.notePatch(pt, "report-failed", "Final email for %s: %v", p.label(r), err)
		p.warnf("%s: final report email for %s failed: %v", patchName(pt), p.label(r), err)
		if p.fail(&r.Failure, "report email", err) {
			r.Final.Email = state.StepFailed
			p.errorf("patch %d: giving up sending report for %s: %v", pt.ID,
				p.label(r), err)
			r.Failure = nil
		}
		return
	}
	r.Failure = nil
	r.Final.MessageID = id
	r.Final.Email = emailStep(p.Cfg.Mode.EmailOff())
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, x := range parts {
		if strings.TrimSpace(x) != "" {
			out = append(out, strings.TrimRight(x, "\n"))
		}
	}
	return strings.Join(out, "\n\n")
}

func (p *Pipeline) sendReport(ctx context.Context, s *state.Series, pt *state.Patch, r *state.Run,
	result, kind, extra string) (string, error) {
	generation := fmt.Sprintf("%s.%d", kind, state.ParseTime(s.Started).Unix())
	// A new dispatch of a workflow restarts attempts from 1.
	if n := dispatchNumber(r); n > 1 {
		generation += fmt.Sprintf(".d%d", n)
	}
	label := p.label(r)
	id := report.MessageID(p.Cfg.Report.MessageIDDomain, s.ID, pt.ID, label, generation,
		max(r.Attempt, 1))
	if p.Cfg.Mode.EmailOff() {
		p.notePatch(pt, "report-skipped", "%s email for %s (%s): email is off.",
			kind, label, result)
		p.infof("%s: not sending the %s email for %s (%s); email mode is off.",
			patchName(pt), kind, label, result)
		return id, nil
	}
	buildURL := r.URL
	if buildURL == "" && pt.Pushed != "" && s.Branch != "" {
		buildURL = p.branchURL(s.Branch)
	}
	var cc []string
	if result != state.ResultSuccess && result != state.ResultPending &&
		p.Cfg.Report.CcSubmitter && p.Cfg.Mode.Email == "live" && s.Submitter.Email != "" {
		cc = append(cc, (&mailAddress{s.Submitter.Name, s.Submitter.Email}).String())
	}
	e, err := report.Compose(report.Input{
		Style:       p.Cfg.Report.Style,
		Test:        p.Cfg.Mode.TestEmail(),
		Label:       label,
		Result:      result,
		Description: p.description(r, result),
		PatchID:     pt.ID,
		PatchName:   pt.Name,
		PatchMsgID:  pt.MsgID,
		PatchURL:    p.pw().PatchWebURL(pt.ID),
		BuildURL:    buildURL,
		Extra:       extra,
		From:        p.Cfg.Report.From,
		To:          p.Cfg.Report.To,
		Cc:          cc,
		MessageID:   id,
		Date:        p.now(),
	})
	if err != nil {
		return "", err
	}
	p.infof("%s: sending the %s report for %s (%s) to %s; Cc: %s. Email mode: %s.",
		patchName(pt), kind, label, result, p.Cfg.Report.To,
		strings.Join(cc, ", "), p.Cfg.Mode.Email)
	if err := p.Mail.Send(ctx, e); err != nil {
		return "", err
	}
	p.endOutage(state.OutageEmail, "a report was sent")
	p.Stats.Emails++
	p.noteRun(pt, r.URL, r.Attempt, "report-sent",
		"Sent %s email for %s (%s). Message-ID: %s.", kind, label, result, id)
	p.infof("%s: %s report for %s (%s) sent. Message-ID: %s.",
		patchName(pt), kind, label, result, id)
	if kind == "final" {
		p.infof("%s: its final Patchwork check will come from the reports-list archive, "+
			"not directly from this email send.", patchName(pt))
	}
	return id, nil
}

type mailAddress struct{ name, email string }

func (a *mailAddress) String() string {
	if a.name == "" {
		return a.email
	}
	return fmt.Sprintf("%q <%s>", a.name, a.email)
}

var logTimestampRe = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z `)

func (p *Pipeline) failureLogs(ctx context.Context, runID int64) (string, error) {
	p.infof("Fetching failed-job details and log excerpts for GitHub run %d.", runID)
	jobs, err := p.GH.Jobs(ctx, runID)
	if err != nil {
		return "", err
	}
	var failed []gh.Job
	for _, j := range jobs {
		if j.Conclusion == "failure" || j.Conclusion == "timed_out" {
			failed = append(failed, j)
		}
	}
	if len(failed) == 0 {
		p.infof("Run %d has no failed or timed-out jobs to include in its report.", runID)
		return "", nil
	}
	var b strings.Builder
	b.WriteString("Failed jobs:\n")
	for _, j := range failed {
		step := ""
		for _, st := range j.Steps {
			if st.Conclusion == "failure" {
				step = st.Name
				break
			}
		}
		if step != "" {
			fmt.Fprintf(&b, "  %s: failed at step %q\n", j.Name, step)
		} else {
			fmt.Fprintf(&b, "  %s: %s\n", j.Name, j.Conclusion)
		}
	}
	maxJobs := p.Cfg.Report.MaxLogJobs
	for i, j := range failed {
		if i >= maxJobs {
			p.infof("Run %d: logs limited to %d failed jobs; omitting %d more.",
				runID, maxJobs, len(failed)-maxJobs)
			fmt.Fprintf(&b, "\nLogs of %d more failed jobs are not included.\n",
				len(failed)-maxJobs)
			break
		}
		p.infof("Fetching log tail for failed job %d (%s) in run %d.", j.ID, j.Name, runID)
		log, err := p.GH.JobLog(ctx, j.ID)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\nLast %d log lines of %q:\n", p.Cfg.Report.LogLines, j.Name)
		b.WriteString(logTail(log, p.Cfg.Report.LogLines))
		b.WriteString("\n")
	}
	return b.String(), nil
}

func logTail(log string, n int) string {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(log, "\r\n", "\n"), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = logTimestampRe.ReplaceAllString(l, "")
	}
	return strings.Join(lines, "\n")
}
