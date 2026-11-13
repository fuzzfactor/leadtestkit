// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"leadtestkit/internal/gh"
	"leadtestkit/internal/ghapp"
	"leadtestkit/internal/httpx"
	"leadtestkit/internal/mail"
	"leadtestkit/internal/report"
	"leadtestkit/internal/state"
)

// When a service the bot needs is unavailable, failures are not the
// patches' fault: they wait without a retry deadline instead, the
// maintainers get an alert, and everything continues once it is
// available again.
// GitHub, for example, may disable Actions on a fork without telling
// anyone: pushes start no runs, dispatches fail, and the settings still
// say Actions are enabled until GitHub Support undoes it.
const (
	// outageProbeInterval is how often one patch is pushed to check
	// whether workflows run again, and the longest retry delay of an
	// operation that failed because of an outage.
	outageProbeInterval = time.Hour
	// outageProbeWait is how long a patch pushed while workflows do not
	// run may go without runs before it waits to be pushed again.
	outageProbeWait = 30 * time.Minute
	outageReminder  = 24 * time.Hour
	// archiveOutageAfter is how long an archive may be unreadable
	// before it counts as an outage; archives are often briefly down.
	archiveOutageAfter = 24 * time.Hour
	// emailOutageAfter is how long sending an email may fail before it
	// counts as an outage, which tolerates brief network problems.
	emailOutageAfter = time.Hour
)

func (p *Pipeline) outage(kind string) *state.Outage {
	return p.Store.Meta.Outages[kind]
}

// workflowsDown reports whether GitHub does not run workflows, which
// pauses pushes.
func (p *Pipeline) workflowsDown() bool {
	return p.outage(state.OutageRuns) != nil || p.outage(state.OutageDispatch) != nil
}

type outageText struct {
	// what is the problem, also the subject of alerts.
	what string
	// effect says what the bot does meanwhile.
	effect string
	// explain helps the maintainers with the cause.
	explain string
	// fixed and after are the subject and the effect of the recovery.
	fixed, after string
}

func (p *Pipeline) describe(kind string, o *state.Outage) outageText {
	repo := p.Cfg.TestRepo.Repo
	switch kind {
	case state.OutageRuns, state.OutageDispatch:
		return outageText{
			what: "workflows do not run on " + repo,
			effect: "The bot pushes no new patches and reports nothing for\n" +
				"patches that got no runs.  Every hour it pushes one patch\n" +
				"to check whether workflows run; patches that got no runs\n" +
				"are pushed again then.",
			explain: "This is what happens when GitHub disables Actions on a\n" +
				"repository, which only GitHub Support can undo; the\n" +
				"repository settings may still show Actions as enabled.",
			fixed: "workflows run again on " + repo,
			after: "The bot pushes patches again, also the ones that got\n" +
				"no runs.",
		}
	case state.OutageGitHub:
		return outageText{
			what: "GitHub refuses the GitHub App of the bot",
			effect: "The bot pushes and monitors nothing on " + repo + " and\n" +
				"keeps all patches where they are.",
			explain: "Check that the App is installed on " + repo + " with\n" +
				"the permissions in SETUP.rst accepted, and that the\n" +
				"LEADTEST_APP_CLIENT_ID and LEADTEST_APP_PRIVATE_KEY secrets\n" +
				"of the bot repository belong to it.",
			fixed: "GitHub accepts the GitHub App of the bot again",
			after: "The bot continues where it stopped.",
		}
	case state.OutagePatchwork:
		project := p.Cfg.Patchwork.Project
		if o != nil && o.Checks {
			return outageText{
				what: "Patchwork refuses the checks of the bot",
				effect: "Checks wait and are posted once it accepts them again;\n" +
					"nothing is lost.  Testing and emails continue.",
				explain: "The Patchwork user of the LEADTEST_PW_TOKEN secret\n" +
					"must be a maintainer of the " + project + " project.",
				fixed: "Patchwork accepts the checks of the bot again",
				after: "The checks that waited are posted now.",
			}
		}
		return outageText{
			what: "Patchwork is unavailable",
			effect: "New series, recheck requests and checks wait until it\n" +
				"is available again; nothing is lost.  Patches already\n" +
				"applied are tested and their reports emailed meanwhile.",
			explain: "Patchwork may be down; it is unreachable for hours at\n" +
				"times.  If it refuses the token (HTTP 401), check the\n" +
				"LEADTEST_PW_TOKEN secret.",
			fixed: "Patchwork is available again",
			after: "The bot continues with what waited.",
		}
	case state.OutageEmail:
		return outageText{
			what: "emails cannot be sent through " + p.Cfg.SMTP.Host,
			effect: "Reports wait and are sent once emails can be sent again;\n" +
				"nothing is lost.",
			explain: "Check the smtp settings and the LEADTEST_SMTP_USERNAME and\n" +
				"LEADTEST_SMTP_PASSWORD secrets.",
			fixed: "emails can be sent again through " + p.Cfg.SMTP.Host,
			after: "The reports that waited are sent now.",
		}
	}
	name, _ := strings.CutSuffix(kind, "-archive")
	effect := "Final Patchwork checks wait until reports can be read\n" +
		"again; nothing is lost."
	if name == devCursor {
		effect = "Recheck requests wait until the list can be read again;\n" +
			"nothing is lost."
	}
	return outageText{
		what:    fmt.Sprintf("the %s archive cannot be read", name),
		effect:  effect,
		explain: "The archive server may be down or have changed.",
		fixed:   fmt.Sprintf("the %s archive can be read again", name),
		after:   "The bot reads what it missed.",
	}
}

func (p *Pipeline) startOutage(kind, reason string) {
	p.startOutageAt(kind, reason, p.now())
}

func (p *Pipeline) startOutageAt(kind, reason string, since time.Time) {
	p.startOutageWith(kind, &state.Outage{Reason: reason}, since)
}

func (p *Pipeline) startOutageWith(kind string, o *state.Outage, since time.Time) {
	// Refused dispatches and pushes without runs are one problem.
	workflows := kind == state.OutageRuns || kind == state.OutageDispatch
	if p.outage(kind) != nil || workflows && p.workflowsDown() {
		return
	}
	o.Since = state.FormatTime(since)
	if kind == state.OutageRuns || kind == state.OutageDispatch {
		o.NextProbe = state.FormatTime(p.now().Add(outageProbeInterval))
	}
	if p.Store.Meta.Outages == nil {
		p.Store.Meta.Outages = map[string]*state.Outage{}
	}
	p.Store.Meta.Outages[kind] = o
	p.outageStarted(kind, o)
	p.waitFor(kind)
	p.markReported(kind)
	t := p.describe(kind, o)
	p.errorf("%s: %s. %s", t.what, o.Reason, oneLine(t.effect))
	p.alert(kind, o, t.what)
}

func (p *Pipeline) endOutage(kind, how string) {
	o := p.outage(kind)
	if o == nil {
		return
	}
	delete(p.Store.Meta.Outages, kind)
	p.retryNow(kind)
	p.available(outageService(kind, o))
	t := p.describe(kind, o)
	p.infof("Outage over, %s: %s. It lasted since %s. %s", t.fixed, how, o.Since,
		oneLine(t.after))
	extra := ""
	if kind == state.OutageRuns || kind == state.OutageDispatch {
		extra = fmt.Sprintf("  Patches waiting to be tested: %d.", p.waitingPatches())
	}
	p.sendAlert(nil, t.fixed, fmt.Sprintf("%s:\n\n  %s\n\nThe outage lasted from %s "+
		"to %s.\n%s%s\n\n%s", upper(t.fixed), how, o.Since, state.FormatTime(p.now()),
		t.after, extra, p.alertLinks(kind)))
}

// retryNow makes everything that waited for an outage be retried now,
// with a fresh retry deadline: the time waited for it does not count.
func (p *Pipeline) retryNow(kind string) {
	now := state.FormatTime(p.now())
	p.eachFailure(func(f *state.Failure) {
		if f.Outage != "" && sameOutage(f.Outage, kind) {
			f.First, f.Next, f.Attempts, f.Outage = now, now, 0, ""
		}
	})
}

// waitFor makes the failures whose errors point at an outage that just
// started wait for it; they may have failed before it started.  They
// are retried at least every outageProbeInterval, like the failures
// during it.
func (p *Pipeline) waitFor(kind string) {
	next := p.now().Add(outageProbeInterval)
	p.eachFailure(func(f *state.Failure) {
		if f.Outage == "" && f.Cause != "" && p.waitsFor(f.Cause) == kind {
			f.Outage = kind
			if state.ParseTime(f.Next).After(next) {
				f.Next = state.FormatTime(next)
			}
		}
	})
}

func (p *Pipeline) eachFailure(fn func(f *state.Failure)) {
	do := func(f *state.Failure) {
		if f != nil {
			fn(f)
		}
	}
	for _, s := range p.Store.Queue {
		do(s.Failure)
		for _, pt := range s.Patches {
			do(pt.Failure)
			for _, r := range pt.Runs {
				do(r.Failure)
				do(r.CheckFailure)
			}
		}
	}
	for _, c := range p.Store.Meta.Archives {
		for _, f := range c.Retry {
			do(f)
		}
	}
}

// sameOutage reports whether two kinds are the same outage; refused
// dispatches and pushes without runs are one.
func sameOutage(a, b string) bool {
	workflows := func(k string) bool {
		return k == state.OutageRuns || k == state.OutageDispatch
	}
	return a == b || workflows(a) && workflows(b)
}

// serviceUsed reports whether the service of an outage is still used;
// outages of services turned off would never end.
func (p *Pipeline) serviceUsed(kind string) bool {
	switch kind {
	case state.OutageEmail:
		return !p.Cfg.Mode.EmailOff()
	case state.OutageArchive(reportsCursor):
		return p.archiveUsed(reportsCursor)
	case state.OutageArchive(devCursor):
		return p.archiveUsed(devCursor)
	case state.OutagePatchwork:
		// Patchwork is always read, but checks may no longer be posted.
		o := p.outage(kind)
		return o == nil || !o.Checks || p.Cfg.Mode.PostChecks()
	}
	return true
}

func (p *Pipeline) archiveUsed(cursor string) bool {
	if cursor == reportsCursor {
		return p.Cfg.Archive.Ingest && p.Reports != nil
	}
	return p.Cfg.Rechecks.Enabled && !p.Cfg.UseCommentEvents() && p.Dev != nil
}

// outageCycle reports the outages once per bot run, checks whether
// GitHub accepts the App again and reminds the maintainers once a day.
func (p *Pipeline) outageCycle(ctx context.Context) {
	// A failure of an archive that is not read cannot count later.
	for cursor, c := range p.Store.Meta.Archives {
		if c.Failing != "" && !p.archiveUsed(cursor) {
			c.Failing = ""
		}
	}
	kinds := make([]string, 0, len(p.Store.Meta.Outages))
	for k := range p.Store.Meta.Outages {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		// Alerts sent in this loop may have ended an outage.
		o := p.outage(kind)
		if o == nil {
			continue
		}
		t := p.describe(kind, o)
		if !p.serviceUsed(kind) {
			delete(p.Store.Meta.Outages, kind)
			p.retryNow(kind)
			p.available(outageService(kind, o))
			p.infof("Forgetting that %s: it is no longer used.", t.what)
			continue
		}
		if kind == state.OutageGitHub {
			tools := "heads/" + p.Cfg.TestRepo.ToolsBranch
			if _, err := p.gh().RefSHA(ctx, tools); err == nil {
				p.endOutage(kind, "it answered a request")
				continue
			} else {
				p.infof("GitHub still refuses the GitHub App: %v", err)
			}
		}
		if !p.reported[kind] {
			p.markReported(kind)
			p.errorf("%s since %s: %s. %s", t.what, o.Since, o.Reason,
				oneLine(t.effect))
		}
		switch alerted := state.ParseTime(o.Alerted); {
		case alerted.IsZero():
			p.alert(kind, o, t.what)
		case p.now().Sub(alerted) >= outageReminder:
			p.alert(kind, o, "reminder: "+t.what)
		}
	}
}

func (p *Pipeline) markReported(kind string) {
	if p.reported == nil {
		p.reported = map[string]bool{}
	}
	p.reported[kind] = true
}

func (p *Pipeline) alert(kind string, o *state.Outage, subject string) {
	t := p.describe(kind, o)
	if kind == state.OutageEmail {
		// The failing bot run is the alert, for maintainers who want
		// alerts.
		o.Alerted = state.FormatTime(p.now())
		if len(p.Cfg.Alerts.To) > 0 {
			p.Stats.Alerts = append(p.Stats.Alerts, subject+" (no email: "+o.Reason+")")
		}
		return
	}
	extra := ""
	if kind == state.OutageRuns || kind == state.OutageDispatch {
		extra = fmt.Sprintf("  Patches waiting to be tested: %d.", p.waitingPatches())
	}
	p.sendAlert(o, subject, fmt.Sprintf("Since %s, %s:\n\n  %s\n\n%s\n\n%s%s\n\n%s",
		o.Since, t.what, o.Reason, t.explain, t.effect, extra, p.alertLinks(kind)))
}

func (p *Pipeline) alertLinks(kind string) string {
	links := fmt.Sprintf("Workflow runs: https://%s/%s/actions\n", p.Cfg.TestRepo.GitHost,
		p.Cfg.TestRepo.Repo)
	if kind == state.OutagePatchwork {
		links = fmt.Sprintf("Patchwork: %s/project/%s/list/\n", p.Cfg.PatchworkWebURL(),
			p.Cfg.Patchwork.Project)
	}
	if p.RunURL != "" {
		links += "Bot run: " + p.RunURL + "\n"
	}
	return links
}

// sendAlert emails the maintainers and records it in the outage, if
// any.  A failed alert is sent again in the next cycle.
func (p *Pipeline) sendAlert(o *state.Outage, subject, body string) {
	now := p.now()
	if len(p.Cfg.Alerts.To) == 0 || p.Alerts == nil {
		p.infof("Not sending the alert %q: alerts.to is empty or mode.email is off.",
			subject)
		if o != nil {
			o.Alerted = state.FormatTime(now)
		}
		return
	}
	subject = strings.TrimSpace("LeadTestKit "+p.Cfg.Name) + ": " + subject
	e, err := report.ComposeAlert(report.Alert{
		From: p.Cfg.Sender(), To: p.Cfg.Alerts.To, Subject: subject, Body: body,
		Date: now, MessageID: fmt.Sprintf("<leadtest.alert.%d@%s>", now.UnixNano(),
			p.Cfg.MessageIDDomain()),
	})
	if err == nil {
		err = p.Alerts.Send(context.Background(), e)
		p.emailSent(err)
	}
	if err != nil {
		p.warnf("Cannot send the alert %q; trying again in the next cycle: %v",
			subject, err)
		// Without reports to send, alerts failing for an hour tell
		// that email cannot be sent.
		if o != nil && mail.IsServerError(err) {
			if o.AlertFailing == "" {
				o.AlertFailing = state.FormatTime(now)
			}
			if now.Sub(state.ParseTime(o.AlertFailing)) >= emailOutageAfter {
				p.startOutage(state.OutageEmail, err.Error())
			}
		}
		return
	}
	if o != nil {
		o.Alerted, o.AlertFailing = state.FormatTime(now), ""
	}
	p.Stats.Alerts = append(p.Stats.Alerts, subject)
	p.infof("Sent the alert %q to %s.", subject, strings.Join(p.Cfg.Alerts.To, ", "))
	p.endOutage(state.OutageEmail, "an alert was sent")
}

// emailSent records the availability of email after sending one.
func (p *Pipeline) emailSent(err error) {
	switch {
	case err == nil:
		p.available(state.ServiceEmail)
	case mail.IsServerError(err):
		p.unavailable(state.ServiceEmail, err)
	}
}

// outageOf tells which outage an error comes from, if any: its cause.
// Checks that Patchwork refuses have the cause ServicePatchworkChecks,
// see waitsFor.
func (p *Pipeline) outageOf(err error) string {
	switch {
	case ghapp.IsAuthError(err) || p.githubUnauthorized(err):
		return state.OutageGitHub
	case mail.IsServerError(err):
		return state.OutageEmail
	case isCheckRefusal(err):
		return state.ServicePatchworkChecks
	case isPWError(err) && pwUnavailable(err):
		return state.OutagePatchwork
	}
	return ""
}

// waitsFor tells the outage going on that a failure with a cause waits
// for, if any.  While Patchwork only refuses checks, its other requests
// work, so only refused checks wait then.
func (p *Pipeline) waitsFor(cause string) string {
	o := p.outage(state.OutagePatchwork)
	switch cause {
	case "":
		return ""
	case state.ServicePatchworkChecks:
		if o != nil {
			return state.OutagePatchwork
		}
		return ""
	case state.OutagePatchwork:
		if o != nil && !o.Checks {
			return state.OutagePatchwork
		}
		return ""
	}
	if p.outage(cause) != nil {
		return cause
	}
	return ""
}

// githubUnauthorized reports GitHub refusing the token of the App.
func (p *Pipeline) githubUnauthorized(err error) bool {
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		return false
	}
	api, err1 := url.Parse(p.Cfg.TestRepo.APIURL)
	u, err2 := url.Parse(se.URL)
	return err1 == nil && err2 == nil && api.Host == u.Host
}

// recordFailure records a failure of an operation and reports whether
// its deadline passed.  Failures caused by an outage start it and wait
// for its end instead, retried at least every outageProbeInterval.
func (p *Pipeline) recordFailure(f **state.Failure, op string, err error,
	deadline time.Duration) bool {
	now := p.now()
	cause := p.outageOf(err)
	kind := cause
	// Patchwork starts its outage itself, see pwFailed; until then its
	// failures count as usual.
	if kind == state.OutagePatchwork || kind == state.ServicePatchworkChecks {
		kind = p.waitsFor(cause)
	}
	if kind == "" {
		if *f != nil && (*f).Outage != "" {
			// The time waited for the outage does not count.
			(*f).First, (*f).Attempts, (*f).Outage = state.FormatTime(now), 0, ""
		}
		done := state.RecordFailure(f, op, err, now, deadline)
		(*f).Cause = cause
		p.logFailure(*f, done)
		return done
	}
	state.RecordFailure(f, op, err, now, 0)
	(*f).Cause = cause
	if next := now.Add(outageProbeInterval); state.ParseTime((*f).Next).After(next) {
		(*f).Next = state.FormatTime(next)
	}
	p.logFailure(*f, false)
	if kind == state.OutageEmail && p.outage(kind) == nil &&
		now.Sub(state.ParseTime((*f).First)) < emailOutageAfter {
		return false
	}
	p.startOutage(kind, err.Error())
	// Only the waiting for an outage that started does not count.
	(*f).Outage = kind
	return false
}

// archiveRead tracks whether the index of an archive can be read; an
// outage starts when it could not for archiveOutageAfter.
func (p *Pipeline) archiveRead(cursor string, c *state.ArchiveCursor, err error) {
	kind := state.OutageArchive(cursor)
	if err == nil {
		c.Failing = ""
		p.available(kind)
		p.endOutage(kind, "its index was read")
		return
	}
	p.unavailable(kind, err)
	now := p.now()
	if c.Failing == "" {
		c.Failing = state.FormatTime(now)
	}
	since := state.ParseTime(c.Failing)
	if now.Sub(since) >= archiveOutageAfter {
		p.startOutageAt(kind, err.Error(), since)
	}
}

func actionsDisabled(err error) bool {
	return httpx.StatusCode(err) == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(err.Error()), "has been disabled")
}

// noRuns reports whether GitHub started none of the workflows of a patch.
func noRuns(pt *state.Patch) bool {
	for _, r := range ciRuns(pt) {
		if r.ID != 0 {
			return false
		}
	}
	return true
}

// dispatchAccepted reports whether GitHub accepted a dispatch of the
// patch, which it does not when Actions are disabled.
func dispatchAccepted(pt *state.Patch) bool {
	for _, r := range ciRuns(pt) {
		if r.Kind == state.RunDispatch && r.Status == "requested" {
			return true
		}
	}
	return false
}

// ciIdle reports whether no workflow at all started on the test
// repository since a time, which tells an outage from a patch whose
// workflows did not start.
func (p *Pipeline) ciIdle(ctx context.Context, since time.Time) (bool, error) {
	runs, err := p.gh().RunsSince(ctx, since)
	return len(runs) == 0, err
}

// waitingPatches counts the patches still to be pushed, including the
// ones that got no runs.
func (p *Pipeline) waitingPatches() int {
	n := 0
	for _, s := range p.Store.Queue {
		for _, pt := range s.Patches {
			switch {
			case pt.Status == state.PatchApplied,
				pt.Status == state.PatchPending && s.Active(),
				pt.Status == state.PatchPushed && noRuns(pt):
				n++
			}
		}
	}
	return n
}

// outageRun ends an outage of runs when GitHub started a workflow after
// it began.
func (p *Pipeline) outageRun(wr gh.WorkflowRun) {
	o := p.outage(state.OutageRuns)
	if o == nil || !wr.CreatedAt.After(state.ParseTime(o.Since)) {
		return
	}
	p.endOutage(state.OutageRuns, fmt.Sprintf("%s run %d started at %s", wr.Name, wr.ID,
		state.FormatTime(wr.CreatedAt)))
}

// resetStuck makes the patches of a series that got no runs while
// workflows do not run wait to be pushed again.  Pushing the same
// commit again starts nothing, so the branch is deleted and created
// again by the next push.
func (p *Pipeline) resetStuck(ctx context.Context, s *state.Series) {
	if !p.workflowsDown() {
		return
	}
	now := p.now()
	var stuck []*state.Patch
	for _, pt := range sortedPatches(s) {
		if pt.Status == state.PatchPushed && noRuns(pt) &&
			now.Sub(state.ParseTime(pt.Pushed)) >= outageProbeWait {
			stuck = append(stuck, pt)
		}
	}
	if len(stuck) == 0 {
		return
	}
	branch := p.branchName(s)
	if err := p.gh().DeleteRef(ctx, "heads/"+branch); err != nil {
		p.warnf("%s: cannot delete branch %s to push its patches again later; "+
			"trying again in the next cycle: %v", seriesName(s), branch, err)
		return
	}
	for _, pt := range stuck {
		var keep []*state.Run
		for _, r := range pt.Runs {
			if r.Kind == state.RunApply {
				keep = append(keep, r)
			}
		}
		pt.Runs = keep
		pt.Status = state.PatchApplied
		pt.Pushed = ""
		pt.Failure = nil
		p.notePatch(pt, "ci-unavailable", "No workflow started; workflows do not run "+
			"on %s. Waiting to be pushed again.", p.Cfg.TestRepo.Repo)
		p.infof("%s: no workflow started; it waits to be pushed again when "+
			"workflows run on %s.", patchName(pt), p.Cfg.TestRepo.Repo)
	}
	pushed := false
	for _, pt := range s.Patches {
		pushed = pushed || pt.Status == state.PatchPushed || pt.Status == state.PatchDone
	}
	if !pushed {
		s.Branch = ""
	}
	p.infof("%s: deleted branch %s; %d patches will be pushed again.", seriesName(s),
		branch, len(stuck))
}

// short422 is the message of a refused request, without the URL.
func short422(err error) string {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		var body struct {
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(se.Body), &body) == nil && body.Message != "" {
			return strings.TrimSuffix(body.Message, ".")
		}
		return se.Body
	}
	return err.Error()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func upper(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
