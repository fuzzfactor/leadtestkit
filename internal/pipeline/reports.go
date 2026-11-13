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
				extra, ok := p.reportFinal(ctx, s, pt, r)
				p.reply(ctx, s, pt, r, extra, ok)
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

// reportFinal sends the final report of a run, and returns the details
// it carried for the reply.
func (p *Pipeline) reportFinal(ctx context.Context, s *state.Series,
	pt *state.Patch, r *state.Run) (string, bool) {
	if r.Status != "completed" || r.Result == "" || r.Final.Email != "" {
		return "", false
	}
	extra := r.Details
	if hasLogs(r) {
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
				return "", false
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
		return extra, true
	}
	r.Failure = nil
	r.Final.MessageID = id
	r.Final.Email = emailStep(p.Cfg.Mode.EmailOff())
	return extra, true
}

// learnPatch keeps what replies need from the patch email.
func learnPatch(pt *state.Patch, d *pw.PatchDetail) {
	pt.Subject = pw.Header(d.Headers, "Subject")
	name, email, _ := pw.Sender(d)
	pt.Sender = &state.Person{Name: name, Email: email}
}

func hasLogs(r *state.Run) bool {
	return r.Result != state.ResultSuccess && r.ID > 0 &&
		(r.Kind == state.RunPush || r.Kind == state.RunDispatch)
}

// reply tells the submitter about a failure or warning in a reply to
// the patch email.  extra are the details of the report when ok, or
// fetched again.  Failures only delay its own retries.
func (p *Pipeline) reply(ctx context.Context, s *state.Series, pt *state.Patch,
	r *state.Run, extra string, ok bool) {
	if r.Reply.Email != "" || r.Final.Email == "" {
		return
	}
	skip := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		p.notePatch(pt, "reply-skipped", "Reply about %s: %s", p.label(r), msg)
		p.infof("%s: not replying to the submitter about %s: %s", patchName(pt),
			p.label(r), msg)
		r.Reply.Email = state.StepSkipped
	}
	switch {
	case r.Result == state.ResultSuccess || r.Result == state.ResultPending:
		r.Reply.Email = state.StepSkipped
		return
	// Other warnings are about runs that did not finish, which is
	// rarely the fault of the patch.
	case r.Result == state.ResultWarning && r.Conclusion != "success":
		conclusion := r.Conclusion
		if conclusion == "" {
			conclusion = "none"
		}
		skip("the workflow did not finish normally (conclusion: %s); that is not for "+
			"the submitter to fix.", conclusion)
		return
	case !p.Cfg.Report.Reply.OnFailure():
		skip("report.reply_to_submitter.mode is %s.", p.Cfg.Report.Reply.Mode)
		return
	case p.Cfg.Mode.EmailOff():
		skip("email mode is off.")
		return
	}
	if !r.ReplyFailure.Due(p.now()) {
		p.infof("%s: delaying the reply about %s until the retry at %s.",
			patchName(pt), p.label(r), r.ReplyFailure.Next)
		return
	}
	if pt.Sender == nil {
		p.infof("%s: fetching the patch for the sender and subject of its email.",
			patchName(pt))
		d, err := p.pw().GetPatch(ctx, pt.ID)
		if err != nil {
			p.notePatch(pt, "reply-failed", "Reply about %s: %v", p.label(r), err)
			p.warnf("%s: cannot fetch the patch to reply about %s: %v", patchName(pt),
				p.label(r), err)
			if p.fail(&r.ReplyFailure, "reply email", err) {
				r.Reply.Email = state.StepFailed
				r.ReplyFailure = nil
			}
			return
		}
		learnPatch(pt, d)
	}
	if pt.Sender.Email == "" {
		skip("the mailing list rewrote the From of the patch email of %s and kept no "+
			"original address.", pt.Sender.Name)
		return
	}
	if !ok {
		extra = r.Details
		if hasLogs(r) {
			logs, err := p.failureLogs(ctx, r.ID)
			if err != nil {
				p.warnf("%s: replying without build logs: %v", patchName(pt), err)
				logs = "Build logs are not available: " + err.Error()
			}
			extra = joinNonEmpty(extra, logs)
		}
	}
	id, err := p.sendReply(ctx, s, pt, r, extra)
	if err != nil {
		p.notePatch(pt, "reply-failed", "Reply about %s: %v", p.label(r), err)
		p.warnf("%s: reply about %s failed: %v", patchName(pt), p.label(r), err)
		if p.fail(&r.ReplyFailure, "reply email", err) {
			r.Reply.Email = state.StepFailed
			r.ReplyFailure = nil
		}
		return
	}
	r.ReplyFailure = nil
	r.Reply.MessageID = id
	r.Reply.Email = state.StepSent
}

func (p *Pipeline) sendReply(ctx context.Context, s *state.Series, pt *state.Patch,
	r *state.Run, extra string) (string, error) {
	label := p.label(r)
	id := report.MessageID(p.Cfg.MessageIDDomain(), s.ID, pt.ID, label,
		p.generation(s, r, "reply"), max(r.Attempt, 1))
	submitter := (&mailAddress{pt.Sender.Name, pt.Sender.Email}).String()
	to, cc := submitter, ""
	if p.replyCc(r) {
		cc = p.Cfg.Report.Reply.Cc.Address
	}
	replyTo := p.Cfg.Report.Reply.ReplyTo
	// Without a list, answers would only reach the bot's mailbox.
	invite := cc != "" || replyTo != ""
	intended := joinAddresses(to, cc)
	if replyTo != "" {
		intended += ", with answers to " + replyTo
	}
	test := p.Cfg.Mode.TestEmail()
	if test {
		to, cc, replyTo = p.Cfg.Report.To, "", ""
	}
	subject := pt.Subject
	if subject == "" {
		subject = pt.Name
	}
	e, err := report.ComposeReply(report.Reply{
		Test:       test,
		From:       p.Cfg.Sender(),
		Name:       pt.Sender.Name,
		To:         to,
		Cc:         cc,
		Subject:    subject,
		PatchMsgID: pt.MsgID,
		PatchURL:   p.pw().PatchWebURL(pt.ID),
		Label:      label,
		Apply:      r.Kind == state.RunApply && r.Result == state.ResultFail,
		Result:     r.Result,
		BuildURL:   p.buildURL(s, pt, r),
		Extra:      extra,
		ReplyTo:    replyTo,
		Invite:     invite,
		Signature:  p.Cfg.Report.FromName,
		Intended:   intended,
		MessageID:  id,
		Date:       p.now(),
	})
	if err != nil {
		return "", err
	}
	p.infof("%s: replying about %s (%s) to %s. Email mode: %s.", patchName(pt), label,
		r.Result, joinAddresses(to, cc), p.Cfg.Mode.Email)
	err = p.Mail.Send(ctx, e)
	p.emailSent(err)
	if err != nil {
		return "", err
	}
	p.endOutage(state.OutageEmail, "a reply was sent")
	p.Stats.Emails++
	p.noteRun(pt, r.URL, r.Attempt, "reply-sent", "Replied about %s (%s) to %s. "+
		"Message-ID: %s.", label, r.Result, joinAddresses(to, cc), id)
	p.infof("%s: reply about %s sent. Message-ID: %s.", patchName(pt), label, id)
	return id, nil
}

// replyCc reports whether report.reply_to_submitter.cc.address is
// copied on the reply about a run.
func (p *Pipeline) replyCc(r *state.Run) bool {
	if p.Cfg.Report.Reply.Cc.Address == "" {
		return false
	}
	list := p.Cfg.Report.Reply.Cc.Workflows
	return len(list) == 0 || containsFold(list, r.Workflow) ||
		r.File != "" && containsFold(list, r.File)
}

func joinAddresses(list ...string) string {
	var out []string
	for _, a := range list {
		if a != "" {
			out = append(out, a)
		}
	}
	return strings.Join(out, ", ")
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
	label := p.label(r)
	id := report.MessageID(p.Cfg.MessageIDDomain(), s.ID, pt.ID, label,
		p.generation(s, r, kind), max(r.Attempt, 1))
	if p.Cfg.Mode.EmailOff() {
		p.notePatch(pt, "report-skipped", "%s email for %s (%s): email is off.",
			kind, label, result)
		p.infof("%s: not sending the %s email for %s (%s); email mode is off.",
			patchName(pt), kind, label, result)
		return id, nil
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
		BuildURL:    p.buildURL(s, pt, r),
		Extra:       extra,
		From:        p.Cfg.Sender(),
		To:          p.Cfg.Report.To,
		MessageID:   id,
		Date:        p.now(),
	})
	if err != nil {
		return "", err
	}
	p.infof("%s: sending the %s report for %s (%s) to %s. Email mode: %s.",
		patchName(pt), kind, label, result, p.Cfg.Report.To, p.Cfg.Mode.Email)
	err = p.Mail.Send(ctx, e)
	p.emailSent(err)
	if err != nil {
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

// generation tells apart the Message-IDs of the emails of a run in each
// test of the series.
func (p *Pipeline) generation(s *state.Series, r *state.Run, kind string) string {
	g := fmt.Sprintf("%s.%d", kind, state.ParseTime(s.Started).Unix())
	// A new dispatch of a workflow restarts attempts from 1.
	if n := dispatchNumber(r); n > 1 {
		g += fmt.Sprintf(".d%d", n)
	}
	return g
}

func (p *Pipeline) buildURL(s *state.Series, pt *state.Patch, r *state.Run) string {
	if r.URL == "" && pt.Pushed != "" && s.Branch != "" {
		return p.branchURL(s.Branch)
	}
	return r.URL
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
	jobs, err := p.gh().Jobs(ctx, runID)
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
		log, err := p.gh().JobLog(ctx, j.ID)
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
