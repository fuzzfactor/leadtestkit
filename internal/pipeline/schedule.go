// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"leadtestkit/internal/maint"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/sched"
	"leadtestkit/internal/state"
)

func (p *Pipeline) schedule(ctx context.Context) {
	if !p.Cfg.Mode.PushLive() {
		return
	}
	now := p.now()
	queued, err := p.queuedJobs(ctx)
	if err != nil {
		p.log().Warn("cannot count queued jobs, not pushing", "err", err)
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
		if s.Hold || !s.Failure.Due(now) {
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
			since := state.ParseTime(s.Discovered)
			cands = append(cands, sched.Candidate{SeriesID: s.ID, Author: authorKey(s),
				Since: since, Ready: ready})
		}
	}
	jobs := p.Cfg.Limits.JobsPerPatch
	if avg := int(p.Store.Meta.Stats.JobsPerPatch + 0.5); avg > 0 {
		jobs = avg
	}
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
	})
	if len(picks) > 0 {
		p.log().Info("scheduling", "queued_jobs", queued, "picks", len(picks))
	}
	for _, id := range picks {
		if ctx.Err() != nil {
			return
		}
		s := bySeries[id]
		if s.Status == state.SeriesQueued || s.Status == state.SeriesWaiting {
			p.prepare(ctx, s)
		}
		if s.Status != state.SeriesTesting {
			continue
		}
		next := p.pushable(s)
		if len(next) == 0 {
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
	total := 0
	seen := map[int64]bool{}
	for _, r := range runs {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		jobs, err := p.GH.Jobs(ctx, r.ID)
		if err != nil {
			return 0, err
		}
		if len(jobs) == 0 && r.Status != "in_progress" {
			total++
			continue
		}
		for _, j := range jobs {
			if j.Status == "queued" || j.Status == "waiting" || j.Status == "pending" {
				total++
			}
		}
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
	if err := p.GH.SetBranch(ctx, s.Branch, pt.SHA); err != nil {
		if p.fail(&pt.Failure, "push", err) {
			s.Finish(state.SeriesError, "push: "+pt.Failure.LastError, now)
			p.errorf("series %d: giving up pushing patch %d: %v", s.ID, pt.ID, err)
		} else {
			p.log().Warn("push failed", "series", s.ID, "patch", pt.ID, "err", err)
		}
		return
	}
	pt.Failure = nil
	pt.Status = state.PatchPushed
	pt.Pushed = state.FormatTime(now)
	p.Stats.Pushed++
	p.log().Info("patch pushed", "series", s.ID, "patch", pt.ID, "branch", s.Branch, "sha", pt.SHA)
	p.dispatch(ctx, s, pt)
}

var dependsRe = regexp.MustCompile(`(?mi)^Depends-on:\s*(series|patch)-(\d+)`)

type patchData struct {
	detail *pw.PatchDetail
	mbox   []byte
}

func (p *Pipeline) prepare(ctx context.Context, s *state.Series) {
	now := p.now()
	retry := func(op string, err error) {
		if p.fail(&s.Failure, op, err) {
			s.Finish(state.SeriesError, op+": "+s.Failure.LastError, now)
			p.errorf("series %d: giving up (%s): %v", s.ID, op, err)
			return
		}
		p.log().Warn("series preparation failed", "series", s.ID, "op", op, "err", err)
	}

	patches := sortedPatches(s)
	data := make([]patchData, len(patches))
	terminal := 0
	for i, pt := range patches {
		d, err := p.PW.GetPatch(ctx, pt.ID)
		if err != nil {
			retry("fetch patch", err)
			return
		}
		if d.PullURL != nil && *d.PullURL != "" {
			s.Finish(state.SeriesSkipped, "pull request", now)
			return
		}
		if terminalPatchStates[d.State] {
			terminal++
		}
		mbox, err := pw.BuildMbox(d)
		if err != nil {
			s.Finish(state.SeriesSkipped, err.Error(), now)
			return
		}
		data[i] = patchData{detail: d, mbox: mbox}
	}
	if terminal == len(patches) {
		s.Finish(state.SeriesSkipped, "patches are no longer active", now)
		return
	}

	base, ok, err := p.dependencyBase(ctx, s, data)
	if err != nil {
		retry("dependency", err)
		return
	}
	if !ok {
		return
	}
	if base == nil {
		base, err = p.selectBase(ctx, s, data)
		if err != nil {
			retry("select base", err)
			return
		}
	}
	if base.SHA == "" {
		sha, err := p.Repo.Resolve(ctx, base.GitURL, base.Branch)
		if err != nil {
			retry("fetch base", err)
			return
		}
		base.SHA = sha
	}

	mboxes := make([][]byte, len(data))
	for i, d := range data {
		mboxes[i] = d.mbox
	}
	shas, failed, output, err := p.Repo.Apply(ctx, base.SHA, mboxes)
	if err != nil {
		retry("apply", err)
		return
	}
	if len(shas) > 0 {
		if err := p.Repo.Push(ctx, shas[len(shas)-1], p.stagingRef(s)); err != nil {
			retry("push staging ref", err)
			return
		}
	}

	s.Failure = nil
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
			if p.Cfg.Report.ApplySuccess {
				pt.Runs = append(pt.Runs, p.applyRun(state.ResultSuccess,
					fmt.Sprintf("Applied on top of %s (%s).", base.Branch, short(base.SHA))))
			}
		case i == failed:
			pt.Status = state.PatchApplyFailed
			pt.Runs = append(pt.Runs, p.applyRun(state.ResultFail, applyFailure(base, output)))
		default:
			pt.Status = state.PatchNotApplied
		}
	}
	p.log().Info("series applied", "series", s.ID, "base", base.Branch, "sha", base.SHA,
		"applied", len(shas), "failed_index", failed)
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
	return fmt.Sprintf("Failed to apply on top of %s (%s):\n\n%s", base.Branch, short(base.SHA), output)
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
func (p *Pipeline) dependencyBase(ctx context.Context, s *state.Series, data []patchData) (*state.Base, bool, error) {
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
		return nil, true, nil
	}
	s.DependsOn = depID
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
		sha, err := p.Repo.ResolveTestRef(ctx, p.stagingRef(dep))
		if err != nil {
			return nil, false, err
		}
		return &state.Base{GitURL: p.Cfg.Upstream.GitURL, Branch: dep.Branch, SHA: sha,
			Why: fmt.Sprintf("depends on series %d", depID)}, true, nil
	}
	if dep != nil && dep.Active() && now.Sub(state.ParseTime(s.Discovered)) < p.Cfg.Limits.CompleteTimeout.D() {
		if s.Status != state.SeriesWaiting {
			p.log().Info("series waits for dependency", "series", s.ID, "depends_on", depID)
		}
		s.Status = state.SeriesWaiting
		s.Reason = fmt.Sprintf("waiting for series %d", depID)
		return nil, false, nil
	}
	s.Reason = fmt.Sprintf("dependency series %d is not available", depID)
	return nil, true, nil
}

func (p *Pipeline) selectBase(ctx context.Context, s *state.Series, data []patchData) (*state.Base, error) {
	def := &state.Base{GitURL: p.Cfg.Upstream.GitURL, Branch: p.Cfg.Upstream.DefaultBranch, Why: "default"}
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
				return &state.Base{GitURL: gitURL, Branch: branch, Why: "subject: " + branch}, nil
			}
		case "delegate":
			if delegate != "" && strings.EqualFold(delegate, r.Delegate) {
				return &state.Base{GitURL: gitURL, Branch: r.Branch, Why: "delegate: " + delegate}, nil
			}
		case "maintainers":
			sha, err := p.Repo.Resolve(ctx, def.GitURL, def.Branch)
			if err != nil {
				return nil, err
			}
			content, err := p.Repo.ReadFile(ctx, sha, r.File)
			if err != nil {
				p.log().Warn("cannot read maintainers file", "file", r.File, "err", err)
				continue
			}
			tree := maint.Parse(string(content)).Select(maint.DiffFiles(diffs...))
			if tree == "" {
				continue
			}
			t, ok := r.Trees[tree]
			if !ok {
				def.Why = "default (tree " + tree + " is not configured)"
				continue
			}
			return &state.Base{GitURL: t.GitURL, Branch: t.Branch, Why: "maintainers: " + tree}, nil
		}
	}
	return def, nil
}

const dispatchTitlePrefix = "leadtest "

func (p *Pipeline) dispatch(ctx context.Context, s *state.Series, pt *state.Patch) {
	if !p.Cfg.Workflows.Custom {
		return
	}
	files, err := p.customWorkflows(ctx)
	if err != nil {
		p.log().Warn("cannot list custom workflows", "err", err)
		return
	}
	last := pt.Number == len(s.Patches)
	for _, f := range files {
		perSeries := contains(p.Cfg.Workflows.CustomPerSeries, f)
		if perSeries && !last {
			continue
		}
		if runByFile(pt, f) != nil {
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
	base := ""
	if s.Base != nil {
		base = s.Base.SHA
	}
	for _, r := range pt.Runs {
		if r.Kind != state.RunDispatch || r.Status != "dispatching" || !r.Failure.Due(p.now()) {
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
		if err := p.GH.Dispatch(ctx, r.File, p.Cfg.TestRepo.ToolsBranch, inputs); err != nil {
			if p.fail(&r.Failure, "dispatch", err) {
				r.Status = "completed"
				r.Result = state.ResultWarning
				r.Details = "Failed to start the workflow: " + r.Failure.LastError
				r.Completed = state.FormatTime(p.now())
			}
			p.log().Warn("dispatch failed", "series", s.ID, "patch", pt.ID, "file", r.File, "err", err)
			continue
		}
		r.Failure = nil
		r.Status = "requested"
		r.Updated = state.FormatTime(p.now())
		p.Stats.Dispatched++
	}
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
		if contains(p.Cfg.Workflows.CustomExclude, f) {
			continue
		}
		data, err := p.GH.File(ctx, ".github/workflows/"+f, p.Cfg.TestRepo.ToolsBranch)
		if err != nil {
			return nil, err
		}
		if hasDispatchTrigger(data) {
			out = append(out, f)
		}
	}
	p.customFiles, p.customLoaded = out, true
	p.log().Info("custom workflows", "files", strings.Join(out, ","))
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
