// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"leadtestkit/internal/gh"
	"leadtestkit/internal/httpx"
	"leadtestkit/internal/report"
	"leadtestkit/internal/state"
)

// GitHub may disable Actions on a fork without telling anyone: pushes
// start no runs, dispatches fail, and the settings still say Actions
// are enabled.  Only GitHub Support can undo it, so the bot pauses,
// alerts the maintainers and checks regularly whether workflows run.
const (
	// outageProbeInterval is how often one patch is pushed to check
	// whether workflows run again.
	outageProbeInterval = time.Hour
	// outageProbeWait is how long a patch pushed during an outage may
	// go without runs before it waits to be pushed again.
	outageProbeWait = 30 * time.Minute
	outageReminder  = 24 * time.Hour
)

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
	runs, err := p.GH.RunsSince(ctx, since)
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

func (p *Pipeline) startOutage(kind, reason string) {
	if p.Store.Meta.Outage != nil {
		return
	}
	now := p.now()
	o := &state.Outage{Kind: kind, Since: state.FormatTime(now), Reason: reason,
		NextProbe: state.FormatTime(now.Add(outageProbeInterval))}
	p.Store.Meta.Outage = o
	p.outageReported = true
	p.errorf("workflows do not run on %s: %s. Pausing pushes; one patch will be "+
		"pushed at %s to check whether they run again", p.Cfg.TestRepo.Repo, reason,
		o.NextProbe)
	p.sendAlert(o, "workflows do not run on "+p.Cfg.TestRepo.Repo, p.outageText(o))
}

func (p *Pipeline) endOutage(how string) {
	o := p.Store.Meta.Outage
	if o == nil {
		return
	}
	p.Store.Meta.Outage = nil
	// Dispatches refused during the outage are sent right away.
	for _, s := range p.Store.Queue {
		for _, pt := range s.Patches {
			for _, r := range pt.Runs {
				if r.Status == "dispatching" && r.Failure != nil &&
					r.Failure.Op == "dispatch" {
					r.Failure = nil
				}
			}
		}
	}
	waiting := p.waitingPatches()
	p.infof("Workflows run again on %s: %s. They did not run since %s; resuming "+
		"pushes with %d patches waiting.", p.Cfg.TestRepo.Repo, how, o.Since, waiting)
	p.sendAlert(nil, "workflows run again on "+p.Cfg.TestRepo.Repo, fmt.Sprintf(
		"Workflows run again on %s:\n\n"+
			"  %s\n\n"+
			"They did not run from %s to %s.\n"+
			"The bot pushes patches again, also the ones that got no runs.\n"+
			"Patches waiting to be tested: %d.\n\n%s",
		p.Cfg.TestRepo.Repo, how, o.Since, state.FormatTime(p.now()), waiting,
		p.alertLinks()))
}

func (p *Pipeline) outageText(o *state.Outage) string {
	return fmt.Sprintf("GitHub does not run the workflows of %s since %s,\n"+
		"when the bot noticed:\n\n"+
		"  %s\n\n"+
		"This is what happens when GitHub disables Actions on a repository,\n"+
		"which only GitHub Support can undo; the repository settings may still\n"+
		"show Actions as enabled.\n\n"+
		"Until workflows run again, the bot pushes no new patches and reports\n"+
		"nothing for patches that got no runs.  Every hour it pushes one patch\n"+
		"to check whether workflows run; patches that got no runs are pushed\n"+
		"again then.  Patches waiting to be tested: %d.\n\n%s",
		p.Cfg.TestRepo.Repo, o.Since, o.Reason, p.waitingPatches(), p.alertLinks())
}

func (p *Pipeline) alertLinks() string {
	links := fmt.Sprintf("Workflow runs: https://%s/%s/actions\n", p.Cfg.TestRepo.GitHost,
		p.Cfg.TestRepo.Repo)
	if p.RunURL != "" {
		links += "Bot run: " + p.RunURL + "\n"
	}
	return links
}

// outageCycle reports an ongoing outage once per bot run and reminds
// the maintainers once a day.
func (p *Pipeline) outageCycle() {
	o := p.Store.Meta.Outage
	if o == nil {
		return
	}
	if !p.outageReported {
		p.outageReported = true
		p.errorf("workflows do not run on %s since %s: %s. Pushes are paused; the next "+
			"check is at %s", p.Cfg.TestRepo.Repo, o.Since, o.Reason, o.NextProbe)
	}
	switch alerted := state.ParseTime(o.Alerted); {
	case alerted.IsZero():
		p.sendAlert(o, "workflows do not run on "+p.Cfg.TestRepo.Repo, p.outageText(o))
	case p.now().Sub(alerted) >= outageReminder:
		p.sendAlert(o, "workflows still do not run on "+p.Cfg.TestRepo.Repo,
			p.outageText(o))
	}
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
		From: p.Cfg.Report.From, To: p.Cfg.Alerts.To, Subject: subject, Body: body,
		Date: now, MessageID: fmt.Sprintf("<leadtest.alert.%d@%s>", now.Unix(),
			p.Cfg.Report.MessageIDDomain),
	})
	if err == nil {
		err = p.Alerts.Send(context.Background(), e)
	}
	if err != nil {
		p.warnf("Cannot send the alert %q; trying again in the next cycle: %v",
			subject, err)
		return
	}
	if o != nil {
		o.Alerted = state.FormatTime(now)
	}
	p.Stats.Alerts = append(p.Stats.Alerts, subject)
	p.infof("Sent the alert %q to %s.", subject, strings.Join(p.Cfg.Alerts.To, ", "))
}

// outageRun ends an outage when GitHub started a workflow after it began.
func (p *Pipeline) outageRun(wr gh.WorkflowRun) {
	o := p.Store.Meta.Outage
	if o == nil || o.Kind != state.OutageRuns ||
		!wr.CreatedAt.After(state.ParseTime(o.Since)) {
		return
	}
	p.endOutage(fmt.Sprintf("%s run %d started at %s", wr.Name, wr.ID,
		state.FormatTime(wr.CreatedAt)))
}

// resetStuck makes the patches of a series that got no runs during an
// outage wait to be pushed again.  Pushing the same commit again starts
// nothing, so the branch is deleted and created again by the next push.
func (p *Pipeline) resetStuck(ctx context.Context, s *state.Series) {
	o := p.Store.Meta.Outage
	if o == nil {
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
	if err := p.GH.DeleteRef(ctx, "heads/"+branch); err != nil {
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
			"on %s since %s. Waiting to be pushed again.", p.Cfg.TestRepo.Repo, o.Since)
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
