// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"leadtestkit/internal/gh"
	"leadtestkit/internal/state"
)

const monitorDeadline = 24 * time.Hour

func (p *Pipeline) monitor(ctx context.Context) {
	dispatched := p.dispatchedRuns(ctx)
	for _, s := range p.Store.Queue {
		if s.Status != state.SeriesTesting {
			continue
		}
		for _, pt := range sortedPatches(s) {
			if ctx.Err() != nil {
				return
			}
			if pt.Status == state.PatchPushed && pt.Failure.Due(p.now()) {
				p.monitorPatch(ctx, s, pt, dispatched)
			} else if pt.Status == state.PatchPushed {
				p.infof("%s: monitoring is delayed until %s after a %s failure.",
					patchName(pt), pt.Failure.Next, pt.Failure.Op)
			}
		}
	}
}

func (p *Pipeline) monitorPatch(ctx context.Context, s *state.Series, pt *state.Patch,
	dispatched map[string]gh.WorkflowRun) {
	runs, err := p.GH.RunsForSHA(ctx, pt.SHA)
	if err != nil {
		p.warnf("%s: cannot list workflow runs: %v", patchName(pt), err)
		done := state.RecordFailure(&pt.Failure, "monitor", err, p.now(), monitorDeadline)
		p.logFailure(pt.Failure, done)
		p.notePatch(pt, "monitor-failed", "Attempt %d: %s. Next retry: %s.",
			pt.Failure.Attempts, pt.Failure.LastError, pt.Failure.Next)
		if done {
			p.abandonRuns(pt, "Lost track of the workflow runs: "+pt.Failure.LastError)
		}
		return
	}
	if pt.Failure != nil && pt.Failure.Op == "monitor" {
		pt.Failure = nil
	}
	p.updatePushRuns(ctx, s, pt, runs)
	p.updateDispatchRuns(ctx, s, pt, dispatched)
	p.checkComplete(ctx, s, pt)
}

func (p *Pipeline) dispatchedRuns(ctx context.Context) map[string]gh.WorkflowRun {
	var since time.Time
	for _, s := range p.Store.Queue {
		for _, pt := range s.Patches {
			for _, r := range pt.Runs {
				if r.Kind == state.RunDispatch && r.Status != "completed" {
					if t := state.ParseTime(pt.Pushed); !t.IsZero() &&
						(since.IsZero() || t.Before(since)) {
						since = t
					}
				}
			}
		}
	}
	if since.IsZero() {
		p.infof("No unfinished custom dispatches need to be matched to GitHub runs.")
		return nil
	}
	p.infof("Looking for dispatched runs on %s since %s.",
		p.Cfg.TestRepo.ToolsBranch, state.FormatTime(since.Add(-time.Hour)))
	runs, err := p.GH.DispatchRuns(ctx, p.Cfg.TestRepo.ToolsBranch, since.Add(-time.Hour))
	if err != nil {
		p.warnf("Cannot list dispatched workflow runs: %v", err)
		return nil
	}
	out := map[string]gh.WorkflowRun{}
	for _, r := range runs {
		id, ok := strings.CutPrefix(r.DisplayTitle, dispatchTitlePrefix)
		if !ok || !p.ownDispatch(r) {
			continue
		}
		if cur, seen := out[id]; !seen || r.ID > cur.ID {
			out[id] = r
		}
	}
	p.infof("Found %d dispatched runs with LeadTestKit request names.", len(out))
	return out
}

func (p *Pipeline) updatePushRuns(ctx context.Context, s *state.Series, pt *state.Patch,
	runs []gh.WorkflowRun) {
	latest := map[string]gh.WorkflowRun{}
	for _, r := range runs {
		var why string
		switch {
		case r.Event != "push":
			why = "it was started by " + r.Event + ", not by a push"
		case r.HeadBranch != s.Branch:
			why = fmt.Sprintf("it ran on branch %s, not %s", r.HeadBranch, s.Branch)
		case r.HeadSHA != pt.SHA:
			why = fmt.Sprintf("it tested commit %s, not %s", short(r.HeadSHA),
				short(pt.SHA))
		}
		if why != "" {
			p.infof("%s: ignoring run %d (%s): %s.", patchName(pt), r.ID, r.Name, why)
			continue
		}
		if cur, ok := latest[r.Name]; !ok || r.ID > cur.ID {
			latest[r.Name] = r
		}
	}
	names := make([]string, 0, len(latest))
	for name := range latest {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		wr := latest[name]
		r := pt.Run(state.RunPush, name)
		if r == nil {
			r = &state.Run{Kind: state.RunPush, Workflow: name}
			pt.Runs = append(pt.Runs, r)
		}
		p.updateRun(ctx, s, pt, r, wr)
	}
}

func (p *Pipeline) updateDispatchRuns(ctx context.Context, s *state.Series, pt *state.Patch,
	runs map[string]gh.WorkflowRun) {
	p.sendDispatches(ctx, s, pt)
	for _, r := range pt.Runs {
		if r.Kind != state.RunDispatch || r.Status == "dispatching" {
			continue
		}
		wr, ok := runs[r.CorrelationID]
		if !ok {
			p.infof("%s: waiting for dispatched workflow %s (request %s) to appear.",
				patchName(pt), r.File, r.CorrelationID)
			continue
		}
		if name := p.workflowName(ctx, r.File); name != "" && name != r.Workflow {
			p.infof("%s: %s is the workflow %s.", patchName(pt), r.File, name)
			r.Workflow = name
		}
		p.updateRun(ctx, s, pt, r, wr)
	}
}

func (p *Pipeline) updateRun(ctx context.Context, s *state.Series, pt *state.Patch, r *state.Run,
	wr gh.WorkflowRun) {
	if r.Status == "completed" && r.Result != "" && wr.RunAttempt <= r.Attempt {
		return
	}
	// After a rerun request the old attempt is still reported until
	// GitHub starts the new one.
	if r.MinAttempt > 0 && wr.RunAttempt <= r.MinAttempt {
		p.infof("%s: %s run %d reports attempt %d; waiting for a newer attempt than %d.",
			patchName(pt), r.Workflow, wr.ID, wr.RunAttempt, r.MinAttempt)
		return
	}
	now := state.FormatTime(p.now())
	// Only changes are logged; the patch's waiting message sums up the rest.
	if r.ID != wr.ID || r.Attempt != wr.RunAttempt || r.Status != wr.Status ||
		r.Conclusion != wr.Conclusion {
		status := wr.Status
		if wr.Conclusion != "" {
			status += ": " + wr.Conclusion
		}
		p.noteRun(pt, wr.HTMLURL, wr.RunAttempt, "workflow-updated",
			"%s run %d, attempt %d: %s.", r.Workflow, wr.ID, wr.RunAttempt, status)
		p.infof("%s: %s run %d, attempt %d: %s.",
			patchName(pt), r.Workflow, wr.ID, wr.RunAttempt, status)
	}
	if r.ID != wr.ID || r.Attempt != wr.RunAttempt || r.Status != wr.Status {
		r.Updated = now
	}
	r.ID = wr.ID
	r.Attempt = wr.RunAttempt
	r.URL = wr.HTMLURL
	r.Status = wr.Status
	r.Conclusion = wr.Conclusion
	if wr.Status != "completed" {
		return
	}
	if wr.Conclusion == "cancelled" && p.Cfg.Workflows.RerunCancelled && r.AutoReruns < 1 &&
		p.Cfg.Mode.PushLive() {
		err := p.rerun(ctx, wr.ID)
		if err == nil {
			p.noteRun(pt, wr.HTMLURL, wr.RunAttempt, "rerun",
				"Restarted cancelled %s run %d, attempt %d.", r.Workflow, wr.ID,
				wr.RunAttempt)
			p.infof("%s: restarted cancelled run %d; this is its one automatic retry.",
				patchName(pt), wr.ID)
			r.AutoReruns++
			r.MinAttempt = wr.RunAttempt
			r.Status = "requested"
			return
		}
		p.warnf("%s: could not restart cancelled run %d: %v", patchName(pt), wr.ID, err)
		p.noteRun(pt, wr.HTMLURL, wr.RunAttempt, "rerun-failed",
			"Cancelled %s run %d, attempt %d: %v", r.Workflow, wr.ID, wr.RunAttempt,
			err)
	}
	result := conclusionResult(wr.Conclusion)
	if result == state.ResultSuccess {
		var ok bool
		if result, ok = p.successResult(ctx, pt, r, wr); !ok {
			return
		}
	}
	r.Result = result
	r.Completed = now
	if r.Pending.Check == "" {
		r.Pending.Check = state.StepSkipped
	}
	if r.Pending.Email == "" {
		r.Pending.Email = state.StepSkipped
	}
	p.infof("%s: workflow %s completed with %s; the report result is %s.",
		patchName(pt), r.Workflow, wr.Conclusion, r.Result)
}

func conclusionResult(c string) string {
	switch c {
	case "success":
		return state.ResultSuccess
	case "failure":
		return state.ResultFail
	}
	return state.ResultWarning
}

func (p *Pipeline) abandonRuns(pt *state.Patch, details string) {
	p.notePatch(pt, "monitor-abandoned", "%s", details)
	p.warnf("%s: marking unfinished workflows as warnings: %s", patchName(pt), details)
	now := state.FormatTime(p.now())
	for _, r := range pt.Runs {
		if r.Status != "completed" || r.Result == "" {
			r.Status = "completed"
			r.Result = state.ResultWarning
			r.Details = details
			r.Completed = now
		}
	}
	if len(pt.Runs) == 0 {
		pt.Runs = append(pt.Runs, &state.Run{Kind: state.RunMissing, Status: "completed",
			Result: state.ResultWarning, Details: details,
			Updated: now, Completed: now})
	}
}

func ciRuns(pt *state.Patch) []*state.Run {
	var out []*state.Run
	for _, r := range pt.Runs {
		if r.Kind != state.RunApply {
			out = append(out, r)
		}
	}
	return out
}

func (p *Pipeline) checkComplete(ctx context.Context, s *state.Series, pt *state.Patch) {
	now := p.now()
	pushed := state.ParseTime(pt.Pushed)
	lim := p.Cfg.Limits
	stamp := state.FormatTime(now)

	warn := func(r *state.Run, details string) {
		p.noteRun(pt, r.URL, r.Attempt, "workflow-warning", "%s: %s", p.label(r), details)
		p.warnf("%s: workflow %s is being marked as a warning: %s",
			patchName(pt), r.Workflow, details)
		r.Status, r.Result = "completed", state.ResultWarning
		r.Completed, r.Details = stamp, details
		if r.Updated == "" {
			r.Updated = stamp
		}
	}
	missing := func(workflow, details string) {
		r := &state.Run{Kind: state.RunMissing, Workflow: workflow}
		warn(r, details)
		pt.Runs = append(pt.Runs, r)
	}

	runs := ciRuns(pt)
	// Dispatches held back by push mode off wait without timing out.
	held := func(r *state.Run) bool {
		return r.Status == "dispatching" && !p.Cfg.Mode.PushLive()
	}
	if now.Sub(pushed) > lim.NoRunTimeout.D() {
		if len(runs) == 0 {
			missing("", "No workflow runs were started for this patch.")
		}
		for _, name := range p.Cfg.Workflows.Expected {
			if pt.Run(state.RunPush, name) == nil &&
				pt.Run(state.RunMissing, name) == nil {
				missing(name, "The workflow did not start for this patch.")
			}
		}
		for _, r := range ciRuns(pt) {
			if r.Kind == state.RunDispatch && r.ID == 0 && r.Status == "requested" {
				warn(r, "The dispatched workflow run was not found.")
			}
		}
	}
	if now.Sub(pushed) > lim.RunTimeout.D() {
		for _, r := range ciRuns(pt) {
			if (r.Status != "completed" || r.Result == "") && !held(r) {
				warn(r, "Timed out waiting for the workflow to finish.")
			}
		}
	}

	runs = ciRuns(pt)
	if len(runs) == 0 {
		p.infof("%s: no CI runs found yet; waiting until %s before declaring them missing.",
			patchName(pt), state.FormatTime(pushed.Add(lim.NoRunTimeout.D())))
		return
	}
	finished := 0
	for _, r := range runs {
		if r.Status == "completed" && r.Result != "" {
			finished++
		}
	}
	var last time.Time
	for _, r := range runs {
		if held(r) {
			p.infof("%s: %s is not dispatched while push mode is %s; waiting.",
				patchName(pt), r.File, p.Cfg.Mode.Push)
			return
		}
		if r.Status != "completed" || r.Result == "" {
			name := r.Workflow
			if name == "" {
				name = r.File
			}
			p.infof("%s: %d of %d workflows finished; waiting for %s, "+
				"timeout at %s.", patchName(pt), finished, len(runs), name,
				state.FormatTime(pushed.Add(lim.RunTimeout.D())))
			return
		}
		if t := state.ParseTime(r.Completed); t.After(last) {
			last = t
		}
	}
	if len(p.Cfg.Workflows.Expected) == 0 {
		// Without a list of expected workflows, wait a little in case
		// more runs show up.
		if now.Sub(last) < lim.GraceAfterRuns.D() ||
			now.Sub(pushed) < lim.GraceAfterRuns.D() {
			until := last.Add(lim.GraceAfterRuns.D())
			if t := pushed.Add(lim.GraceAfterRuns.D()); t.After(until) {
				until = t
			}
			p.infof("%s: all known runs finished; waiting until %s for late workflows.",
				patchName(pt), state.FormatTime(until))
			return
		}
	} else {
		for _, name := range p.Cfg.Workflows.Expected {
			if pt.Run(state.RunPush, name) == nil &&
				pt.Run(state.RunMissing, name) == nil {
				p.infof("%s: waiting for expected workflow %s to appear.",
					patchName(pt), name)
				return
			}
		}
	}
	for _, r := range pt.Runs {
		if !r.Settled() {
			p.infof("%s: waiting for the final %s report to be sent.",
				patchName(pt), p.label(r))
			return
		}
	}
	pt.Status = state.PatchDone
	p.notePatch(pt, "done", "Testing and reporting are complete.")
	p.recordJobs(ctx, pt)
	p.infof("%s: testing and reporting are complete.", patchName(pt))
}

// recordJobs keeps a moving average of jobs per patch, which the
// scheduler uses to estimate the queue growth of a push.
func (p *Pipeline) recordJobs(ctx context.Context, pt *state.Patch) {
	n := 0
	for _, r := range pt.Runs {
		if r.ID == 0 {
			continue
		}
		jobs, err := p.GH.Jobs(ctx, r.ID)
		if err != nil {
			p.warnf("%s: cannot count jobs in run %d; keeping the old job estimate: %v",
				patchName(pt), r.ID, err)
			return
		}
		n += len(jobs)
	}
	if n == 0 {
		return
	}
	st := &p.Store.Meta.Stats
	if st.JobsPerPatch == 0 {
		st.JobsPerPatch = float64(n)
	} else {
		st.JobsPerPatch = st.JobsPerPatch*0.8 + float64(n)*0.2
	}
	st.JobsPerPatch = float64(int(st.JobsPerPatch*10+0.5)) / 10
	p.infof("%s: counted %d CI jobs; the moving estimate is now %.1f jobs per patch.",
		patchName(pt), n, st.JobsPerPatch)
}

func hasDispatchTrigger(data []byte) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return false
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value != "on" && k.Value != "true" {
			continue
		}
		switch v.Kind {
		case yaml.ScalarNode:
			return v.Value == "workflow_dispatch"
		case yaml.SequenceNode:
			for _, e := range v.Content {
				if e.Value == "workflow_dispatch" {
					return true
				}
			}
		case yaml.MappingNode:
			for j := 0; j < len(v.Content); j += 2 {
				if v.Content[j].Value == "workflow_dispatch" {
					return true
				}
			}
		}
	}
	return false
}
