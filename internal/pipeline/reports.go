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

// report sends pending and final reports for all runs.
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
				if !r.Failure.Due(p.now()) {
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

func (p *Pipeline) reportPending(ctx context.Context, s *state.Series, pt *state.Patch, r *state.Run) {
	if r.Kind != state.RunPush && r.Kind != state.RunDispatch {
		return
	}
	if r.ID == 0 || r.Status == "completed" || r.Status == "requested" {
		return
	}
	if r.Pending.Check == "" {
		if !p.Cfg.Report.PendingCheck {
			r.Pending.Check = state.StepSkipped
		} else {
			chk := pw.NewCheck{
				State:       state.ResultPending,
				TargetURL:   p.branchURL(s.Branch),
				Context:     report.Context(p.label(r)),
				Description: p.description(r, state.ResultPending),
			}
			if !p.Cfg.Mode.PostChecks() {
				p.log().Info("would post check", "patch", pt.ID, "state", chk.State,
					"context", chk.Context, "target_url", chk.TargetURL)
				r.Pending.Check = state.StepLogged
			} else if err := p.PW.CreateCheck(ctx, pt.ID, chk); err != nil {
				p.log().Warn("pending check failed", "patch", pt.ID, "err", err)
				if p.fail(&r.Failure, "pending check", err) {
					r.Pending.Check = state.StepFailed
					r.Failure = nil
				}
				return
			} else {
				r.Pending.Check = state.StepPosted
				p.Stats.Checks++
			}
		}
	}
	if r.Pending.Email == "" {
		if !p.Cfg.Report.PendingEmail {
			r.Pending.Email = state.StepSkipped
			return
		}
		id, err := p.sendReport(ctx, s, pt, r, state.ResultPending, "pending", "")
		if err != nil {
			p.log().Warn("pending email failed", "patch", pt.ID, "err", err)
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
}

func emailStep(off bool) string {
	if off {
		return state.StepSkipped
	}
	return state.StepSent
}

func (p *Pipeline) reportFinal(ctx context.Context, s *state.Series, pt *state.Patch, r *state.Run) {
	if r.Status != "completed" || r.Result == "" || r.Final.Email != "" {
		return
	}
	extra := r.Details
	if r.Result != state.ResultSuccess && r.ID > 0 && (r.Kind == state.RunPush || r.Kind == state.RunDispatch) {
		logs, err := p.failureLogs(ctx, r.ID)
		if err != nil {
			attempts := 0
			if r.Failure != nil && r.Failure.Op == "logs" {
				attempts = r.Failure.Attempts
			}
			if attempts+1 < logFetchAttempts {
				p.fail(&r.Failure, "logs", err)
				p.log().Warn("cannot fetch logs, will retry", "run", r.ID, "err", err)
				return
			}
			logs = "Build logs are not available: " + err.Error()
		}
		extra = joinNonEmpty(extra, logs)
	}
	id, err := p.sendReport(ctx, s, pt, r, r.Result, "final", extra)
	if err != nil {
		p.log().Warn("report email failed", "patch", pt.ID, "err", err)
		if p.fail(&r.Failure, "report email", err) {
			r.Final.Email = state.StepFailed
			p.errorf("patch %d: giving up sending report for %s: %v", pt.ID, p.label(r), err)
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
	generation := state.ParseTime(s.Started).Unix()
	label := p.label(r)
	id := report.MessageID(p.Cfg.Report.MessageIDDomain, s.ID, pt.ID, label,
		fmt.Sprintf("%s.%d", kind, generation), max(r.Attempt, 1))
	if p.Cfg.Mode.EmailOff() {
		p.log().Info("email disabled, not sending report", "patch", pt.ID, "label", label,
			"result", result, "kind", kind)
		return id, nil
	}
	buildURL := r.URL
	if buildURL == "" && pt.SHA != "" {
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
		PatchURL:    p.PW.PatchWebURL(pt.ID),
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
	if err := p.Mail.Send(ctx, e); err != nil {
		return "", err
	}
	p.Stats.Emails++
	p.log().Info("report sent", "patch", pt.ID, "label", label, "result", result, "kind", kind)
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

// failureLogs summarizes failed jobs of a run with the end of their logs.
func (p *Pipeline) failureLogs(ctx context.Context, runID int64) (string, error) {
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
			fmt.Fprintf(&b, "\nLogs of %d more failed jobs are not included.\n", len(failed)-maxJobs)
			break
		}
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
