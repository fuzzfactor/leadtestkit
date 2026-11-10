// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"leadtestkit/internal/archive"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/report"
	"leadtestkit/internal/state"
)

const commentEvents = "patch-comment-created"

var recheckRe = regexp.MustCompile(`(?m)^Recheck-request:[ \t]*(.+)$`)

// ParseRecheck returns whether a comment requests a recheck by label,
// and the other tokens of its Recheck-request lines, which may be
// Patchwork check names like "github-robot-_Build_and_Test".
func ParseRecheck(content string, labels []string) (bool, []string) {
	all := false
	var names []string
	for _, m := range recheckRe.FindAllStringSubmatch(content, -1) {
		for _, tok := range strings.FieldsFunc(m[1], func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t'
		}) {
			label := false
			for _, l := range labels {
				if strings.EqualFold(tok, l) {
					label = true
				}
			}
			if label {
				all = true
			} else {
				names = append(names, tok)
			}
		}
	}
	return all, names
}

type tracked struct {
	series *state.Series
	patch  *state.Patch
}

func (p *Pipeline) rechecks(ctx context.Context) {
	if !p.Cfg.Rechecks.Enabled {
		p.infof("Not looking for recheck requests: rechecks are disabled.")
		return
	}
	now := p.now()
	recent, err := p.Store.Recent(now, p.Cfg.Rechecks.Window.D())
	if err != nil {
		p.errorf("rechecks: %v", err)
		return
	}
	patches := map[int]tracked{}
	for _, s := range recent {
		if s.Base == nil {
			continue
		}
		for _, pt := range s.Patches {
			if pt.SHA != "" {
				patches[pt.ID] = tracked{s, pt}
			}
		}
	}

	if p.Cfg.UseCommentEvents() {
		p.infof("Looking for rechecks in Patchwork comment events for %d tracked patches.",
			len(patches))
		p.commentsFromEvents(ctx, patches)
	} else if p.Dev != nil {
		p.infof("Looking for rechecks through the dev archive (%d tracked patches).",
			len(patches))
		p.commentsFromArchive(ctx, patches)
	} else {
		p.infof("Cannot find rechecks: comment events are off and no dev archive is set.")
	}

	ids := make([]int, 0, len(patches))
	for id := range patches {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	labels := p.Cfg.RecheckLabels()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		t := patches[id]
		if !t.patch.CommentsPending {
			continue
		}
		if p.pwSkipped("fetching comments") {
			return
		}
		p.infof("%s: fetching comments after activity was detected.", patchName(t.patch))
		comments, err := p.pw().GetPatchComments(ctx, id)
		if err != nil {
			p.warnf("%s: cannot fetch comments: %v", patchName(t.patch), err)
			continue
		}
		t.patch.CommentsPending = false
		for _, c := range comments {
			if c.ID <= t.patch.CommentsSeen {
				p.infof("%s: comment %d already processed; skipping it.",
					patchName(t.patch), c.ID)
				continue
			}
			t.patch.CommentsSeen = c.ID
			all, names := ParseRecheck(c.Content, labels)
			checks := p.matchChecks(t.patch, names)
			if all || len(checks) > 0 {
				target := strings.Join(checks, ", ")
				if all {
					target = "all eligible workflows"
				}
				p.notePatch(t.patch, "recheck-requested", "Comment %d: recheck %s.",
					c.ID, target)
				if all {
					p.infof("%s: comment %d requests a recheck by bot label.",
						patchName(t.patch), c.ID)
				} else {
					p.infof("%s: comment %d requests rechecks of %s.",
						patchName(t.patch), c.ID,
						strings.Join(checks, ", "))
				}
			} else {
				p.infof("%s: comment %d does not request our workflows.",
					patchName(t.patch), c.ID)
			}
			t.patch.Recheck = t.patch.Recheck || all
			for _, c := range checks {
				if !containsFold(t.patch.RecheckChecks, c) {
					t.patch.RecheckChecks = append(t.patch.RecheckChecks, c)
				}
			}
		}
	}
	for _, id := range ids {
		if t := patches[id]; (t.patch.Recheck || len(t.patch.RecheckChecks) > 0) &&
			t.patch.Failure.Due(now) {
			p.doRecheck(ctx, t.series, t.patch)
		} else if (t.patch.Recheck || len(t.patch.RecheckChecks) > 0) &&
			t.patch.Failure != nil {
			p.infof("%s: recheck is waiting until %s after a %s failure.",
				patchName(t.patch), t.patch.Failure.Next, t.patch.Failure.Op)
		}
	}
}

func (p *Pipeline) commentsFromEvents(ctx context.Context, patches map[int]tracked) {
	if p.pwSkipped("reading comment events") {
		return
	}
	opts := pw.WalkOptions{Category: commentEvents, Cursor: p.Store.Meta.Events[commentEvents]}
	if opts.Cursor == 0 {
		opts.Cutoff = p.now().Add(-24 * time.Hour)
		opts.MaxPages = 3
	}
	p.infof("Reading comment events after saved event %d.", opts.Cursor)
	cursor, err := p.pw().WalkEvents(ctx, opts, func(ev pw.Event) error {
		if pl, ok := ev.Payload.(*pw.PatchCommentCreatedPayload); ok {
			if t, ok := patches[pl.Patch.ID]; ok {
				p.infof("Comment event %d: marking patch %d for a comment scan.",
					ev.ID, pl.Patch.ID)
				t.patch.CommentsPending = true
			} else {
				p.infof("Ignoring event %d: patch %d is not tracked for rechecks.",
					ev.ID, pl.Patch.ID)
			}
		} else {
			p.infof("Ignoring comment event %d: no patch-comment payload.", ev.ID)
		}
		return nil
	})
	if cursor > p.Store.Meta.Events[commentEvents] {
		p.Store.Meta.Events[commentEvents] = cursor
	}
	if err != nil {
		p.warnf("Could not finish reading comment events: %v", err)
	}
	p.infof("Comment-event scan finished; saved cursor: %d.",
		p.Store.Meta.Events[commentEvents])
}

// commentsFromArchive matches new dev list subjects against tracked
// patches, for Patchwork instances without comment events.
func (p *Pipeline) commentsFromArchive(ctx context.Context, patches map[int]tracked) {
	if p.devScanned {
		p.infof("Not rescanning the dev archive: it was already read in this invocation.")
		return
	}
	c := p.Store.Cursor(devCursor)
	months, next, ok := p.newMessages(ctx, p.Dev, devCursor)
	p.devScanned = ok
	var msgs []archive.ArchiveMessage
	for _, mm := range months {
		msgs = append(msgs, mm.msgs...)
		if len(mm.msgs) > 0 {
			c.Months[mm.key] = mm.msgs[len(mm.msgs)-1].Number
		}
	}
	if ok || c.LastMonth != "" {
		c.LastMonth = next
	}
	if len(msgs) == 0 {
		p.infof("No new dev-list messages to match against tracked patch subjects.")
		return
	}
	names := map[int]string{}
	for id, t := range patches {
		names[id] = t.patch.Name
	}
	for _, id := range archive.MatchPatchSubjects(msgs, names) {
		p.infof("Dev-list activity matches patch %d; checking comments for rechecks.", id)
		patches[id].patch.CommentsPending = true
	}
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func rerunnable(r *state.Run) bool {
	return r.Kind == state.RunPush || r.Kind == state.RunDispatch
}

// matchChecks keeps the names that are check names of runs of the patch,
// as posted to Patchwork; other labs' names are ignored.
func (p *Pipeline) matchChecks(pt *state.Patch, names []string) []string {
	var out []string
	for _, r := range pt.Runs {
		if !rerunnable(r) {
			continue
		}
		check := report.Context(p.label(r))
		if containsFold(names, check) && !containsFold(out, check) {
			out = append(out, check)
		}
	}
	return out
}

func (p *Pipeline) doRecheck(ctx context.Context, s *state.Series, pt *state.Patch) {
	now := p.now()
	done := func() {
		pt.Recheck = false
		pt.RecheckChecks = nil
	}
	failedOnly := p.Cfg.Workflows.RerunJobs == "failed"
	var rerun, redispatch []*state.Run
	explicit := map[*state.Run]bool{}
	for _, r := range pt.Runs {
		if !rerunnable(r) {
			what := "the result of applying the patch"
			if r.Kind == state.RunMissing {
				what = "a warning about a workflow that never started"
			}
			p.infof("%s: not rerunning %s; it is %s, not a workflow run.",
				patchName(pt), p.label(r), what)
			continue
		}
		if r.Status != "completed" {
			p.infof("%s: not rerunning %s; it has not finished yet (%s).",
				patchName(pt), p.label(r), r.Status)
			continue
		}
		explicit[r] = containsFold(pt.RecheckChecks, report.Context(p.label(r)))
		if !explicit[r] && !pt.Recheck {
			p.infof("%s: not rerunning %s; the request names other checks.",
				patchName(pt), p.label(r))
			continue
		}
		if !explicit[r] && failedOnly && r.Result == state.ResultSuccess {
			p.infof("%s: not rerunning %s; it passed, and workflows.rerun_jobs "+
				"only reruns failures.", patchName(pt), p.label(r))
			continue
		}
		switch {
		case r.ID > 0:
			rerun = append(rerun, r)
		case r.Kind == state.RunDispatch:
			// The workflow never started, so there is no run to restart.
			redispatch = append(redispatch, r)
		}
	}
	if len(rerun)+len(redispatch) == 0 {
		p.notePatch(pt, "recheck-skipped", "No completed workflows qualify.")
		p.infof("%s: no workflows qualify for this recheck; clearing the request.",
			patchName(pt))
		done()
		return
	}
	if !p.Cfg.Mode.PushLive() {
		p.notePatch(pt, "recheck-skipped", "Push mode is %s; clearing the request.",
			p.Cfg.Mode.Push)
		p.infof("%s: would restart %d workflows, but push mode is %s; clearing request.",
			patchName(pt), len(rerun)+len(redispatch), p.Cfg.Mode.Push)
		done()
		return
	}
	reset := func(r *state.Run) {
		r.Status = "requested"
		r.Result, r.Conclusion, r.Completed, r.Details = "", "", "", ""
		r.Pending, r.Final = state.Report{}, state.Report{}
		r.AutoReruns = 0
		r.Failure, r.CheckFailure = nil, nil
		r.Updated = state.FormatTime(now)
	}
	for _, r := range rerun {
		p.infof("%s: restarting workflow %s, GitHub run %d.",
			patchName(pt), p.label(r), r.ID)
		var err error
		if explicit[r] && r.Result == state.ResultSuccess {
			err = p.gh().Rerun(ctx, r.ID, false)
		} else {
			err = p.rerun(ctx, r.ID)
		}
		if err != nil {
			p.noteRun(pt, r.URL, r.Attempt, "rerun-failed", "%s run %d, attempt %d: %v",
				p.label(r), r.ID, r.Attempt, err)
			if p.fail(&pt.Failure, "recheck", err) {
				done()
				pt.Failure = nil
				p.errorf("patch %d: giving up rerunning workflows: %v", pt.ID, err)
			}
			p.warnf("%s: could not restart %s: %v", patchName(pt), p.label(r), err)
			return
		}
		r.MinAttempt = r.Attempt
		p.noteRun(pt, r.URL, r.Attempt, "rerun", "Restarted %s run %d after attempt %d.",
			p.label(r), r.ID, r.Attempt)
		reset(r)
		p.infof("%s: rerun accepted for %s; waiting for a newer attempt.",
			patchName(pt), p.label(r))
	}
	for _, r := range redispatch {
		p.infof("%s: %s has no run ID; requesting a new dispatch.",
			patchName(pt), p.label(r))
		r.CorrelationID = nextCorrelationID(r.CorrelationID)
		reset(r)
		r.Status = "dispatching"
	}
	var keep []*state.Run
	for _, r := range pt.Runs {
		if r.Kind != state.RunMissing {
			keep = append(keep, r)
		}
	}
	pt.Runs = keep
	pt.Failure = nil
	done()
	pt.Status = state.PatchPushed
	pt.Pushed = state.FormatTime(now)
	if !s.Active() || s.Status != state.SeriesTesting {
		if p.Store.Find(s.ID) == nil {
			p.infof("%s: reopening from history for the recheck.", seriesName(s))
			if err := p.Store.Reopen(s); err != nil {
				p.errorf("series %d: reopen: %v", s.ID, err)
			}
		}
		s.Status = state.SeriesTesting
		s.Reason = ""
		s.Finished = ""
	}
	p.sendDispatches(ctx, s, pt)
	p.Stats.Rechecks++
	p.notePatch(pt, "recheck-started", "%d reruns accepted; %d workflows queued for dispatch.",
		len(rerun), len(redispatch))
	p.infof("%s: recheck started; %d reruns accepted and %d workflows queued for redispatch.",
		patchName(pt), len(rerun), len(redispatch))
}

// dispatchNumber tells which dispatch of its workflow a run is, from the
// suffix nextCorrelationID bumps, or 0 for runs that are not dispatched.
func dispatchNumber(r *state.Run) int {
	if r.Kind != state.RunDispatch {
		return 0
	}
	i := strings.LastIndexByte(r.CorrelationID, '-')
	n, err := strconv.Atoi(r.CorrelationID[i+1:])
	if i < 0 || err != nil {
		return 1
	}
	return n
}

// nextCorrelationID bumps the attempt suffix, so that a new dispatch
// gets a run name of its own.
func nextCorrelationID(id string) string {
	i := strings.LastIndexByte(id, '-')
	n, err := strconv.Atoi(id[i+1:])
	if i < 0 || err != nil {
		return id + "-2"
	}
	return id[:i+1] + strconv.Itoa(n+1)
}
