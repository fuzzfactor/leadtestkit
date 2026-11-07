// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"leadtestkit/internal/gh"
	"leadtestkit/internal/maint"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/sched"
	"leadtestkit/internal/state"
)

func (p *Pipeline) schedule(ctx context.Context) {
	if !p.Cfg.Mode.PushLive() {
		p.infof("Not scheduling patches: push mode is %q; live pushes are required.",
			p.Cfg.Mode.Push)
		return
	}
	now := p.now()
	queued, err := p.queuedJobs(ctx)
	if err != nil {
		p.warnf("Not scheduling: GitHub's queued jobs could not be counted: %v", err)
		return
	}

	inflight := map[string]int{}
	var cands []sched.Candidate
	bySeries := map[int]*state.Series{}
	for _, s := range p.Store.Queue {
		bySeries[s.ID] = s
		for _, pt := range s.Patches {
			if pt.Status == state.PatchPushed {
				inflight[authorKey(s)]++
			}
		}
		if s.Hold {
			p.infof("%s: not scheduling because it is on manual hold.", seriesName(s))
			continue
		}
		if !s.Failure.Due(now) {
			p.infof("%s: not scheduling until the %s retry at %s.",
				seriesName(s), s.Failure.Op, s.Failure.Next)
			continue
		}
		ready := 0
		switch s.Status {
		case state.SeriesQueued, state.SeriesWaiting:
			ready = len(s.Patches)
		case state.SeriesTesting:
			ready = len(p.pushable(s))
		}
		if ready > 0 {
			p.infof("%s: %d patches are scheduling candidates for %s.",
				seriesName(s), ready, authorKey(s))
			since := state.ParseTime(s.Discovered)
			cands = append(cands, sched.Candidate{SeriesID: s.ID, Author: authorKey(s),
				Since: since, Ready: ready})
		} else if s.Active() {
			p.infof("%s: no patches are ready to push (series status: %s).",
				seriesName(s), s.Status)
			for _, pt := range s.Patches {
				if !pt.Failure.Due(now) {
					p.infof("%s: waiting until %s to retry %s.",
						patchName(pt), pt.Failure.Next, pt.Failure.Op)
				}
			}
		}
	}
	jobs := p.Cfg.Limits.JobsPerPatch
	if avg := int(p.Store.Meta.Stats.JobsPerPatch + 0.5); avg > 0 {
		jobs = avg
	}
	total := 0
	for _, n := range inflight {
		total += n
	}
	p.infof("CI capacity: %d queued jobs (limit %d), %d patches in flight (limit %d); "+
		"estimating %d jobs per additional patch.", queued, p.Cfg.Limits.MaxQueuedJobs,
		total, p.Cfg.Limits.MaxInflightPatches, jobs)
	picks := sched.Pick(sched.Input{
		Limits: sched.Limits{
			MaxQueuedJobs:      p.Cfg.Limits.MaxQueuedJobs,
			MaxInflightPatches: p.Cfg.Limits.MaxInflightPatches,
			PerAuthorMin:       p.Cfg.Limits.PerAuthorMin,
			PerAuthorMaxShare:  p.Cfg.Limits.PerAuthorMaxShare,
			JobsPerPatch:       jobs,
		},
		QueuedJobs: queued,
		Inflight:   inflight,
		Candidates: cands,
		Explain:    p.infof,
	})
	p.infof("Scheduling decision: selected %d patch slots from %d candidate series.",
		len(picks), len(cands))
	for _, id := range picks {
		if ctx.Err() != nil {
			return
		}
		s := bySeries[id]
		if s.Status == state.SeriesQueued || s.Status == state.SeriesWaiting {
			p.prepare(ctx, s)
		}
		if s.Status != state.SeriesTesting {
			p.infof("%s: not pushing after preparation; its status is %s (%s).",
				seriesName(s), s.Status, s.Reason)
			continue
		}
		next := p.pushable(s)
		if len(next) == 0 {
			p.infof("%s: no pushable patches remain in this cycle.", seriesName(s))
			continue
		}
		p.push(ctx, s, next[0])
	}
}

func (p *Pipeline) queuedJobs(ctx context.Context) (int, error) {
	runs, err := p.GH.ActiveRuns(ctx)
	if err != nil {
		return 0, err
	}
	p.infof("GitHub returned %d active workflow runs; counting jobs waiting for a runner.",
		len(runs))
	total := 0
	seen := map[int64]bool{}
	for _, r := range runs {
		if seen[r.ID] {
			p.infof("Run %d was already counted; ignoring its duplicate listing.", r.ID)
			continue
		}
		seen[r.ID] = true
		jobs, err := p.GH.Jobs(ctx, r.ID)
		if err != nil {
			return 0, err
		}
		if len(jobs) == 0 && r.Status != "in_progress" {
			p.infof("Run %d (%s) has no jobs yet; reserving one queued-job slot.",
				r.ID, r.Name)
			total++
			continue
		}
		waiting := 0
		for _, j := range jobs {
			if j.Status == "queued" || j.Status == "waiting" || j.Status == "pending" {
				total++
				waiting++
			}
		}
		p.infof("Run %d (%s): %d of %d jobs are waiting for a runner.",
			r.ID, r.Name, waiting, len(jobs))
	}
	return total, nil
}

func (p *Pipeline) pushable(s *state.Series) []*state.Patch {
	var out []*state.Patch
	for _, pt := range sortedPatches(s) {
		switch pt.Status {
		case state.PatchPushed, state.PatchDone:
			continue
		case state.PatchApplied:
			if pt.Failure.Due(p.now()) {
				out = append(out, pt)
			}
			continue
		}
		break
	}
	return out
}

func (p *Pipeline) push(ctx context.Context, s *state.Series, pt *state.Patch) {
	now := p.now()
	pushFailed := func(err error) {
		if p.fail(&pt.Failure, "push", err) {
			p.finishSeries(s, state.SeriesError, "push: "+pt.Failure.LastError, now)
			p.errorf("series %d: giving up pushing patch %d: %v", s.ID, pt.ID, err)
		} else {
			p.warnf("%s: could not push for series %d: %v", patchName(pt), s.ID, err)
		}
		p.notePatch(pt, "push-failed", "Attempt %d: %s. Next retry: %s.",
			pt.Failure.Attempts, pt.Failure.LastError, pt.Failure.Next)
	}
	// After a lost state the branch may already be at this or a later
	// patch; moving it would start the workflows again.
	p.infof("%s: checking branch %s before pushing commit %s.",
		patchName(pt), s.Branch, short(pt.SHA))
	cur, err := p.GH.RefSHA(ctx, "heads/"+s.Branch)
	if err != nil {
		pushFailed(err)
		return
	}
	if cur != "" {
		ordered := sortedPatches(s)
		head := -1
		for i, x := range ordered {
			if x.SHA == cur {
				head = i
			}
		}
		if head >= 0 && ordered[head].Number >= pt.Number {
			for _, x := range ordered[:head+1] {
				if x.Status == state.PatchApplied {
					p.notePatch(x, "push-recovered", "Already on %s at %s.",
						s.Branch, cur)
					p.markPushed(ctx, s, x, now)
					p.infof("%s: already on %s; not triggering CI again.",
						patchName(x), s.Branch)
				}
			}
			return
		}
	}
	p.infof("%s: updating branch %s to %s to trigger push workflows.",
		patchName(pt), s.Branch, short(pt.SHA))
	if err := p.GH.SetBranch(ctx, s.Branch, pt.SHA); err != nil {
		pushFailed(err)
		return
	}
	p.Stats.Pushed++
	p.notePatch(pt, "pushed", "Updated branch %s to %s.", s.Branch, pt.SHA)
	p.infof("%s: pushed to %s at %s; waiting for GitHub workflows.",
		patchName(pt), s.Branch, short(pt.SHA))
	p.markPushed(ctx, s, pt, now)
}

func (p *Pipeline) markPushed(ctx context.Context, s *state.Series,
	pt *state.Patch, now time.Time) {
	pt.Failure = nil
	pt.Status = state.PatchPushed
	pt.Pushed = state.FormatTime(now)
	p.dispatch(ctx, s, pt)
}

var dependsRe = regexp.MustCompile(`(?mi)^Depends-on:\s*(series|patch)-(\d+)`)

type patchData struct {
	detail *pw.PatchDetail
	mbox   []byte
}

func (p *Pipeline) prepare(ctx context.Context, s *state.Series) {
	now := p.now()
	p.infof("%s: preparing %d patches for testing.", seriesName(s), len(s.Patches))
	retry := func(op string, err error) {
		if p.fail(&s.Failure, op, err) {
			p.finishSeries(s, state.SeriesError, op+": "+s.Failure.LastError, now)
			p.errorf("series %d: giving up (%s): %v", s.ID, op, err)
			return
		}
		p.noteSeries(s, "prepare-failed", "%s, attempt %d: %s. Next retry: %s.",
			op, s.Failure.Attempts, s.Failure.LastError, s.Failure.Next)
		p.warnf("%s: preparation failed while trying to %s: %v", seriesName(s), op, err)
	}

	patches := sortedPatches(s)
	data := make([]patchData, len(patches))
	terminal := 0
	for i, pt := range patches {
		p.infof("%s: fetching patch %d of %d and building its email for git am.",
			patchName(pt), i+1, len(patches))
		d, err := p.PW.GetPatch(ctx, pt.ID)
		if err != nil {
			retry("fetch patch", err)
			return
		}
		if d.PullURL != nil && *d.PullURL != "" {
			p.infof("%s: skipping because patch %d is a pull request.",
				seriesName(s), pt.ID)
			p.finishSeries(s, state.SeriesSkipped, "pull request", now)
			return
		}
		if terminalPatchStates[d.State] {
			p.infof("%s: Patchwork state is %s; counting it as no longer active.",
				patchName(pt), d.State)
			terminal++
		}
		mbox, err := pw.BuildMbox(d)
		if err != nil {
			p.infof("%s: skipping because patch %d cannot be made into an email: %v",
				seriesName(s), pt.ID, err)
			p.finishSeries(s, state.SeriesSkipped, err.Error(), now)
			return
		}
		data[i] = patchData{detail: d, mbox: mbox}
	}
	if terminal == len(patches) {
		p.infof("%s: skipping because all patches are in terminal Patchwork states.",
			seriesName(s))
		p.finishSeries(s, state.SeriesSkipped, "patches are no longer active", now)
		return
	}

	var reused []string
	var base *state.Base
	var err error
	if !s.Reapply {
		p.infof("%s: checking recovery refs for commits applied by an earlier run.",
			seriesName(s))
		base, reused, err = p.reuseApplied(ctx, s, patches)
		if err != nil {
			retry("check applied patches", err)
			return
		}
	} else {
		p.infof("%s: a fresh application was requested; not reusing recovery refs.",
			seriesName(s))
	}
	if base == nil {
		var ok bool
		base, ok, err = p.dependencyBase(ctx, s, data)
		if err != nil {
			retry("dependency", err)
			return
		}
		if !ok {
			return
		}
	}
	if base == nil {
		base, err = p.selectBase(ctx, s, data)
		if err != nil {
			retry("select base", err)
			return
		}
	}
	if base.SHA == "" {
		p.infof("%s: resolving base branch %s from %s (%s).",
			seriesName(s), base.Branch, base.GitURL, base.Why)
		sha, err := p.Repo.Resolve(ctx, base.GitURL, base.Branch)
		if err != nil {
			retry("fetch base", err)
			return
		}
		base.SHA = sha
	}
	p.infof("%s: selected base %s at %s: %s.",
		seriesName(s), base.Branch, short(base.SHA), base.Why)
	p.noteSeries(s, "base-selected", "%s at %s: %s.", base.Branch, base.SHA, base.Why)

	shas, failed, output := reused, -1, ""
	if len(reused) < len(data) {
		top := base.SHA
		if len(reused) > 0 {
			top = reused[len(reused)-1]
		}
		var mboxes [][]byte
		for _, d := range data[len(reused):] {
			mboxes = append(mboxes, d.mbox)
		}
		p.infof("%s: applying %d patches on top of %s.",
			seriesName(s), len(mboxes), short(top))
		applied, f, out, err := p.Repo.Apply(ctx, top, mboxes)
		if err != nil {
			retry("apply", err)
			return
		}
		shas = append(shas, applied...)
		if f >= 0 {
			failed, output = len(reused)+f, out
		}
	}
	if len(shas) > 0 {
		refs := map[string]string{p.stagingRef(s): shas[len(shas)-1]}
		for i, sha := range shas {
			refs[p.patchRef(patches[i])] = sha
		}
		p.infof("%s: publishing %d recovery refs before updating the test branch.",
			seriesName(s), len(refs))
		if err := p.Repo.PushRefs(ctx, refs); err != nil {
			retry("push staging refs", err)
			return
		}
		p.infof("%s: saved %d recovery refs; these refs do not trigger push workflows.",
			seriesName(s), len(refs))
	}

	s.Failure = nil
	s.Reapply = false
	s.Base = base
	s.Branch = fmt.Sprintf("%s%d", p.Cfg.TestRepo.BranchPrefix, s.ID)
	s.Status = state.SeriesTesting
	s.Started = state.FormatTime(now)
	p.Stats.Applied++
	for i, pt := range patches {
		pt.Runs = nil
		switch {
		case failed < 0 || i < failed:
			pt.SHA = shas[i]
			pt.Status = state.PatchApplied
			if i < len(reused) {
				p.notePatch(pt, "apply-recovered", "Reused saved commit %s.",
					pt.SHA)
			} else {
				p.notePatch(pt, "applied", "Commit %s on base %s (%s).",
					pt.SHA, base.Branch, base.SHA)
			}
			p.infof("%s: applied as commit %s.", patchName(pt), short(pt.SHA))
			if p.Cfg.Report.ApplySuccess {
				pt.Runs = append(pt.Runs, p.applyRun(state.ResultSuccess,
					fmt.Sprintf("Applied on top of %s (%s).",
						base.Branch, short(base.SHA))))
			}
		case i == failed:
			p.warnf("%s: failed to apply on %s at %s:\n%s",
				patchName(pt), base.Branch, short(base.SHA), output)
			pt.Status = state.PatchApplyFailed
			p.notePatch(pt, "apply-failed", "%s", applyFailure(base, output))
			pt.Runs = append(pt.Runs,
				p.applyRun(state.ResultFail, applyFailure(base, output)))
		default:
			p.infof("%s: not applied because an earlier patch failed.", patchName(pt))
			pt.Status = state.PatchNotApplied
			p.notePatch(pt, "not-applied", "An earlier patch failed to apply.")
		}
	}
	p.infof("%s: preparation finished; %d of %d patches applied (%d reused).",
		seriesName(s), len(shas), len(patches), len(reused))
}

// reuseApplied finds patches applied by an earlier attempt whose
// results were lost, e.g. when a run was killed.  Reusing the same
// commits keeps branch updates from starting the workflows again.
func (p *Pipeline) reuseApplied(ctx context.Context, s *state.Series,
	patches []*state.Patch) (*state.Base, []string, error) {
	names := make([]string, len(patches))
	for i, pt := range patches {
		names[i] = p.patchRef(pt)
	}
	found, err := p.Repo.TestRefs(ctx, names)
	if err != nil || len(found) == 0 {
		if err == nil {
			p.infof("%s: no saved patch commits found; a fresh apply is needed.",
				seriesName(s))
		}
		return nil, nil, err
	}
	var shas []string
	base := ""
	for i, name := range names {
		sha, ok := found[name]
		if !ok {
			p.infof("%s: recovery ref %s is missing; ending commit reuse here.",
				seriesName(s), name)
			break
		}
		parent, err := p.Repo.Parent(ctx, sha)
		if err != nil {
			return nil, nil, err
		}
		if i == 0 {
			base = parent
		} else if parent != shas[i-1] {
			p.infof("%s: saved patch chain is broken; ending reuse before patch %d.",
				seriesName(s), patches[i].ID)
			break
		}
		shas = append(shas, sha)
	}
	if len(shas) == 0 {
		return nil, nil, nil
	}
	p.infof("%s: reusing %d previously applied patches to avoid duplicate CI runs.",
		seriesName(s), len(shas))
	return &state.Base{GitURL: p.Cfg.Upstream.GitURL, SHA: base,
		Why: "applied by an earlier attempt"}, shas, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func applyFailure(base *state.Base, output string) string {
	const maxDetails = 4000
	if len(output) > maxDetails {
		output = "...\n" + output[len(output)-maxDetails:]
	}
	return fmt.Sprintf("Failed to apply on top of %s (%s):\n\n%s", base.Branch,
		short(base.SHA), output)
}

func (p *Pipeline) applyRun(result, details string) *state.Run {
	now := state.FormatTime(p.now())
	return &state.Run{
		Kind:      state.RunApply,
		Workflow:  p.Cfg.Report.ApplyLabel,
		Status:    "completed",
		Result:    result,
		Details:   details,
		Updated:   now,
		Completed: now,
	}
}

// dependencyBase handles "Depends-on:".  It returns ok=false while the
// series has to wait for its dependency.
func (p *Pipeline) dependencyBase(ctx context.Context, s *state.Series,
	data []patchData) (*state.Base, bool, error) {
	var text []string
	if s.CoverID > 0 {
		cover, err := p.PW.GetCover(ctx, s.CoverID)
		if err != nil {
			return nil, false, err
		}
		text = append(text, cover.Content)
	}
	if len(data) > 0 {
		text = append(text, data[0].detail.Content)
	}
	depID := 0
	for _, t := range text {
		for _, m := range dependsRe.FindAllStringSubmatch(t, -1) {
			id, _ := strconv.Atoi(m[2])
			if strings.EqualFold(m[1], "patch") {
				d, err := p.PW.GetPatch(ctx, id)
				if err != nil {
					return nil, false, err
				}
				if len(d.Series) == 0 {
					continue
				}
				id = d.Series[0].ID
			}
			depID = id
		}
	}
	if depID == 0 || depID == s.ID {
		p.infof("%s: no external series dependency was found.", seriesName(s))
		return nil, true, nil
	}
	s.DependsOn = depID
	p.infof("%s: requires dependency series %d; looking for its applied commits.",
		seriesName(s), depID)
	now := p.now()
	recent, err := p.Store.Recent(now, p.Cfg.Limits.HistoryWindow.D())
	if err != nil {
		return nil, false, err
	}
	var dep *state.Series
	for _, x := range recent {
		if x.ID == depID {
			dep = x
		}
	}
	var last *state.Patch
	if dep != nil && dep.Base != nil {
		for _, pt := range sortedPatches(dep) {
			if pt.SHA != "" {
				last = pt
			}
		}
	}
	if last != nil {
		p.infof("%s: using dependency series %d as the base.", seriesName(s), depID)
		sha, err := p.Repo.ResolveTestRef(ctx, p.stagingRef(dep))
		if err != nil {
			return nil, false, err
		}
		return &state.Base{GitURL: p.Cfg.Upstream.GitURL, Branch: dep.Branch, SHA: sha,
			Why: fmt.Sprintf("depends on series %d", depID)}, true, nil
	}
	if dep != nil && dep.Active() &&
		now.Sub(state.ParseTime(s.Discovered)) < p.Cfg.Limits.CompleteTimeout.D() {
		if s.Status != state.SeriesWaiting {
			p.noteSeries(s, "dependency-wait", "Waiting for series %d to be applied.",
				depID)
		}
		p.infof("%s: waiting for dependency series %d to be applied.", seriesName(s), depID)
		s.Status = state.SeriesWaiting
		s.Reason = fmt.Sprintf("waiting for series %d", depID)
		return nil, false, nil
	}
	s.Reason = fmt.Sprintf("dependency series %d is not available", depID)
	p.warnf("%s: dependency series %d is unavailable; falling back to base-selection rules.",
		seriesName(s), depID)
	return nil, true, nil
}

func (p *Pipeline) selectBase(ctx context.Context, s *state.Series,
	data []patchData) (*state.Base, error) {
	def := &state.Base{GitURL: p.Cfg.Upstream.GitURL,
		Branch: p.Cfg.Upstream.DefaultBranch, Why: "default"}
	names := []string{s.Name}
	delegate := ""
	var diffs []string
	for _, d := range data {
		names = append(names, d.detail.Name)
		if d.detail.Delegate != nil && delegate == "" {
			delegate = d.detail.Delegate.Username
		}
		diffs = append(diffs, d.detail.Diff)
	}
	for i := range p.Cfg.Rules.Base {
		r := &p.Cfg.Rules.Base[i]
		p.infof("%s: checking base-selection rule %d (%s).", seriesName(s), i+1, r.Type)
		gitURL := r.GitURL
		if gitURL == "" {
			gitURL = p.Cfg.Upstream.GitURL
		}
		switch r.Type {
		case "subject":
			for _, n := range names {
				m := r.Regexp().FindStringSubmatchIndex(n)
				if m == nil {
					continue
				}
				branch := string(r.Regexp().ExpandString(nil, r.Branch, n, m))
				return &state.Base{GitURL: gitURL, Branch: branch,
					Why: "subject: " + branch}, nil
			}
		case "delegate":
			if delegate != "" && strings.EqualFold(delegate, r.Delegate) {
				return &state.Base{GitURL: gitURL, Branch: r.Branch,
					Why: "delegate: " + delegate}, nil
			}
		case "maintainers":
			sha, err := p.Repo.Resolve(ctx, def.GitURL, def.Branch)
			if err != nil {
				return nil, err
			}
			content, err := p.Repo.ReadFile(ctx, sha, r.File)
			if err != nil {
				p.warnf("%s: cannot read %s; trying other base rules: %v",
					seriesName(s), r.File, err)
				continue
			}
			tree := maint.Parse(string(content)).Select(maint.DiffFiles(diffs...))
			if tree == "" {
				p.infof("%s: no unique maintainer tree matches changed files.",
					seriesName(s))
				continue
			}
			t, ok := r.Trees[tree]
			if !ok {
				p.infof("%s: maintainer tree %s has no base; keeping the default.",
					seriesName(s), tree)
				def.Why = "default (tree " + tree + " is not configured)"
				continue
			}
			return &state.Base{GitURL: t.GitURL, Branch: t.Branch,
				Why: "maintainers: " + tree}, nil
		}
		p.infof("%s: base-selection rule %d did not select a base; trying the next rule.",
			seriesName(s), i+1)
	}
	p.infof("%s: no base rule selected a branch; using default branch %s.",
		seriesName(s), def.Branch)
	return def, nil
}

const dispatchTitlePrefix = "leadtest "

func (p *Pipeline) dispatch(ctx context.Context, s *state.Series, pt *state.Patch) {
	if !p.Cfg.Workflows.Custom {
		p.infof("%s: custom workflow dispatch is disabled.", patchName(pt))
		return
	}
	files, err := p.customWorkflows(ctx)
	if err != nil {
		p.warnf("%s: cannot list custom workflows: %v", patchName(pt), err)
		return
	}
	last := pt.Number == len(s.Patches)
	for _, f := range files {
		perSeries := contains(p.Cfg.Workflows.CustomPerSeries, f)
		if perSeries && !last {
			p.infof("%s: not dispatching %s; it runs only on a series' last patch.",
				patchName(pt), f)
			continue
		}
		if runByFile(pt, f) != nil {
			p.infof("%s: %s already has a tracked dispatch; not creating another.",
				patchName(pt), f)
			continue
		}
		pt.Runs = append(pt.Runs, &state.Run{
			Kind:          state.RunDispatch,
			Workflow:      f,
			File:          f,
			CorrelationID: correlationID(s.ID, pt.ID, f, 1),
			Status:        "dispatching",
			Updated:       state.FormatTime(p.now()),
		})
	}
	p.sendDispatches(ctx, s, pt)
}

func correlationID(seriesID, patchID int, file string, attempt int) string {
	slug := strings.TrimSuffix(strings.TrimSuffix(file, ".yml"), ".yaml")
	return fmt.Sprintf("%d-%d-%s-%d", seriesID, patchID, slug, attempt)
}

func (p *Pipeline) sendDispatches(ctx context.Context, s *state.Series, pt *state.Patch) {
	if !p.Cfg.Mode.PushLive() {
		p.infof("%s: not sending workflow dispatches; push mode is %s.",
			patchName(pt), p.Cfg.Mode.Push)
		return
	}
	base := ""
	if s.Base != nil {
		base = s.Base.SHA
	}
	var existing map[string]bool
	for _, r := range pt.Runs {
		if r.Kind != state.RunDispatch || r.Status != "dispatching" {
			continue
		}
		if !r.Failure.Due(p.now()) {
			p.infof("%s: dispatch of %s is waiting until %s to retry.",
				patchName(pt), r.File, r.Failure.Next)
			continue
		}
		// A run that was dispatched before the state was lost, or whose
		// dispatch request failed after reaching GitHub, has our name.
		if existing == nil {
			var err error
			existing, err = p.dispatchedTitles(ctx, s)
			if err != nil {
				p.warnf("%s: cannot check dispatches; not risking duplicates: %v",
					seriesName(s), err)
				return
			}
		}
		if existing[r.CorrelationID] {
			p.notePatch(pt, "dispatch-recovered", "Found existing %s dispatch (%s).",
				r.File, r.CorrelationID)
			p.infof("%s: found an existing dispatch for %s (%s); not sending it again.",
				patchName(pt), r.File, r.CorrelationID)
			r.Status = "requested"
			r.Updated = state.FormatTime(p.now())
			continue
		}
		inputs := map[string]string{
			"sha":            pt.SHA,
			"base_sha":       base,
			"branch":         s.Branch,
			"series_id":      strconv.Itoa(s.ID),
			"patch_id":       strconv.Itoa(pt.ID),
			"patch_index":    strconv.Itoa(pt.Number),
			"correlation_id": r.CorrelationID,
		}
		p.infof("%s: dispatching %s from %s for commit %s (request %s).",
			patchName(pt), r.File, p.Cfg.TestRepo.ToolsBranch,
			short(pt.SHA), r.CorrelationID)
		if err := p.GH.Dispatch(ctx, r.File, p.Cfg.TestRepo.ToolsBranch,
			inputs); err != nil {
			if p.fail(&r.Failure, "dispatch", err) {
				r.Status = "completed"
				r.Result = state.ResultWarning
				r.Details = "Failed to start the workflow: " + r.Failure.LastError
				r.Completed = state.FormatTime(p.now())
			}
			p.notePatch(pt, "dispatch-failed", "%s, attempt %d: %s.",
				r.File, r.Failure.Attempts, r.Failure.LastError)
			p.warnf("%s: could not dispatch %s: %v", patchName(pt), r.File, err)
			continue
		}
		r.Failure = nil
		r.Status = "requested"
		r.Updated = state.FormatTime(p.now())
		// The workflow timeouts count from the push; a dispatch sent in a
		// later cycle (after a retry or with push mode off) restarts them.
		if pushed := state.ParseTime(pt.Pushed); p.now().Sub(pushed) >=
			p.Cfg.Runner.LoopInterval.D() {
			pt.Pushed = state.FormatTime(p.now())
			p.infof("%s: restarting workflow timeouts; "+
				"the dispatch was delayed since %s.",
				patchName(pt), state.FormatTime(pushed))
		}
		p.Stats.Dispatched++
		p.notePatch(pt, "dispatched", "%s from %s for commit %s (request %s).",
			r.File, p.Cfg.TestRepo.ToolsBranch, pt.SHA, r.CorrelationID)
		p.infof("%s: GitHub accepted the %s dispatch; waiting for the run to appear.",
			patchName(pt), r.File)
	}
}

func (p *Pipeline) dispatchedTitles(ctx context.Context, s *state.Series) (map[string]bool, error) {
	runs, err := p.GH.DispatchRuns(ctx, p.Cfg.TestRepo.ToolsBranch,
		state.ParseTime(s.Discovered).Add(-time.Hour))
	if err != nil {
		return nil, err
	}
	titles := map[string]bool{}
	for _, r := range runs {
		if id, ok := strings.CutPrefix(r.DisplayTitle, dispatchTitlePrefix); ok &&
			p.ownDispatch(r) {
			titles[id] = true
		}
	}
	return titles, nil
}

// ownDispatch reports whether the bot started a dispatched run.
// Workflows of tested patches can dispatch runs with the same names, to
// fake the results of other patches.
func (p *Pipeline) ownDispatch(r gh.WorkflowRun) bool {
	want := ""
	if p.Actor != nil {
		want = p.Actor()
	}
	if want == "" || strings.EqualFold(r.Actor.Login, want) {
		return true
	}
	p.warnf("Ignoring dispatched run %d (%s): it was started by %s, not by %s.",
		r.ID, r.DisplayTitle, r.Actor.Login, want)
	return false
}

func (p *Pipeline) customWorkflows(ctx context.Context) ([]string, error) {
	if p.customLoaded {
		return p.customFiles, nil
	}
	files, err := p.GH.WorkflowFiles(ctx, p.Cfg.TestRepo.ToolsBranch)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		// The tools branch also has the project's own workflows.
		if !strings.HasPrefix(f, p.Cfg.Workflows.CustomPrefix) ||
			contains(p.Cfg.Workflows.CustomExclude, f) {
			if contains(p.Cfg.Workflows.CustomExclude, f) {
				p.infof("Not using workflow %s: excluded in config.yaml.", f)
			} else {
				p.infof("Not using workflow %s: name does not start with %s.",
					f, p.Cfg.Workflows.CustomPrefix)
			}
			continue
		}
		data, err := p.GH.File(ctx, ".github/workflows/"+f, p.Cfg.TestRepo.ToolsBranch)
		if err != nil {
			return nil, err
		}
		if hasDispatchTrigger(data) {
			out = append(out, f)
		} else {
			p.infof("Not using custom workflow %s: no workflow_dispatch trigger.", f)
		}
	}
	p.customFiles, p.customLoaded = out, true
	p.infof("Found %d dispatchable custom workflows on %s: %s.",
		len(out), p.Cfg.TestRepo.ToolsBranch, strings.Join(out, ", "))
	return out, nil
}

func runByFile(pt *state.Patch, file string) *state.Run {
	for _, r := range pt.Runs {
		if r.Kind == state.RunDispatch && r.File == file {
			return r
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
