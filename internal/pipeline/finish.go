// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"leadtestkit/internal/gitrepo"
	"leadtestkit/internal/state"
)

const maxRefDeletes = 50

// syncTools keeps the tools branch, the default branch of the test
// repository, close to upstream.  The resulting push runs the project's workflows
// there, which fills the caches that series branches can restore; runs
// only see caches of their own and of the default branch.
func (p *Pipeline) syncTools(ctx context.Context) {
	interval := p.Cfg.TestRepo.SyncInterval.D()
	if p.syncDone {
		p.infof("Not syncing the tools branch: already checked in this invocation.")
		return
	}
	if !p.Cfg.Mode.PushLive() {
		p.infof("Not syncing the tools branch: push mode is %s.", p.Cfg.Mode.Push)
		return
	}
	if interval <= 0 {
		p.infof("Not syncing the tools branch: the sync interval is disabled.")
		return
	}
	p.syncDone = true
	now := p.now()
	if last := state.ParseTime(p.Store.Meta.ToolsSynced); !last.IsZero() &&
		now.Sub(last) < interval {
		p.infof("The tools branch was synced at %s; the next sync is due at %s.",
			state.FormatTime(last), state.FormatTime(last.Add(interval)))
		return
	}
	branch := p.Cfg.TestRepo.ToolsBranch
	up := p.Cfg.Upstream
	p.infof("Rebasing branch %s of the test repository onto upstream %s to refresh "+
		"shared CI caches.", branch, up.DefaultBranch)
	res, err := p.Repo.SyncBranch(ctx, up.GitURL, up.DefaultBranch, branch)
	var conflict *gitrepo.ConflictError
	switch {
	case errors.As(err, &conflict):
		// Retrying before the next interval would not help.
		p.errorf("cannot rebase branch %s of the test repository onto upstream %s, "+
			"rebase it by hand: %v", branch, up.DefaultBranch, err)
	case err != nil:
		p.warnf("Cannot sync branch %s of the test repository; will retry in a later "+
			"run: %v", branch, err)
		return
	case res.New == res.Old:
		p.infof("Branch %s of the test repository already contains upstream; its %d "+
			"own commits are on top.", branch, res.Commits)
	default:
		p.infof("Branch %s of the test repository is now upstream %s with its %d own "+
			"commits on top, at %s.", branch, up.DefaultBranch, res.Commits,
			short(res.New))
		p.customLoaded = false
	}
	p.Store.Meta.ToolsSynced = state.FormatTime(now)
}

func patchFinal(pt *state.Patch) bool {
	switch pt.Status {
	case state.PatchDone, state.PatchNotApplied, state.PatchSkipped:
	case state.PatchApplyFailed:
	default:
		return false
	}
	for _, r := range pt.Runs {
		if !r.Settled() {
			return false
		}
	}
	return true
}

// unfinished counts what keeps a series from being done: patches still
// being tested, and reports of tested patches not sent yet.
func unfinished(s *state.Series) (testing, reports int) {
	for _, pt := range s.Patches {
		switch pt.Status {
		case state.PatchDone, state.PatchNotApplied, state.PatchSkipped,
			state.PatchApplyFailed:
		default:
			testing++
			continue
		}
		for _, r := range pt.Runs {
			if !r.Settled() {
				reports++
			}
		}
	}
	return testing, reports
}

func (p *Pipeline) finish(ctx context.Context) {
	now := p.now()
	var testing, reports []string
	defer func() {
		if len(testing) > 0 {
			p.infof("Series with patches still being tested: %s.",
				joinList(testing, 10))
		}
		if len(reports) > 0 {
			p.infof("Series waiting only for reports to be sent: %s.",
				joinList(reports, 10))
		}
	}()
	for _, s := range append([]*state.Series(nil), p.Store.Queue...) {
		if s.Status == state.SeriesTesting {
			if t, r := unfinished(s); t > 0 {
				testing = append(testing, fmt.Sprintf("%d (%d of %d)", s.ID, t,
					len(s.Patches)))
			} else if r > 0 {
				reports = append(reports, fmt.Sprintf("%d (%d)", s.ID, r))
			} else {
				p.finishSeries(s, state.SeriesDone, "", now)
				p.infof("%s: all patches and reports are complete.", seriesName(s))
			}
		}
		if s.Active() {
			continue
		}
		if s.Finished == "" {
			s.Finished = state.FormatTime(now)
		}
		p.infof("%s: moving to history with status %s (%s).",
			seriesName(s), s.Status, s.Reason)
		if err := p.Store.Archive(s); err != nil {
			p.errorf("series %d: archive: %v", s.ID, err)
			continue
		}
		p.Stats.Finished++
	}
}

// gc only deletes refs of series known to have finished long ago; the
// test repository may be shared with other tools.
func (p *Pipeline) gc(ctx context.Context) {
	if p.gcDone {
		p.infof("Not cleaning old refs: already checked in this invocation.")
		return
	}
	if !p.Cfg.Mode.PushLive() {
		p.infof("Not cleaning old refs: push mode is %s.", p.Cfg.Mode.Push)
		return
	}
	if p.Cfg.Limits.BranchRetention <= 0 {
		p.infof("Not cleaning old refs: branch retention is disabled.")
		return
	}
	p.gcDone = true
	cutoff := p.now().Add(-p.Cfg.Limits.BranchRetention.D())
	p.infof("Looking for refs belonging to series that finished before %s.",
		state.FormatTime(cutoff))
	months, err := p.Store.HistoryMonths()
	if err != nil {
		p.warnf("Cannot list history months for ref cleanup: %v", err)
		return
	}
	expired := map[int]bool{}
	expiredPatches := map[int]bool{}
	for _, m := range months {
		t, err := time.Parse("2006-01", m)
		if err != nil || t.After(cutoff) || t.Before(cutoff.AddDate(0, -6, 0)) {
			continue
		}
		list, err := p.Store.History(m)
		if err != nil {
			p.warnf("Cannot read history month %s for ref cleanup: %v", m, err)
			return
		}
		for _, s := range list {
			if f := state.ParseTime(s.Finished); !f.IsZero() && f.Before(cutoff) {
				expired[s.ID] = true
				for _, pt := range s.Patches {
					expiredPatches[pt.ID] = true
				}
			}
		}
	}
	for _, s := range p.Store.Queue {
		delete(expired, s.ID)
		for _, pt := range s.Patches {
			delete(expiredPatches, pt.ID)
		}
	}
	if len(expired) == 0 {
		p.infof("No series are old enough for ref cleanup in the inspected history window.")
		return
	}
	deleted := 0
	staging := strings.TrimPrefix(p.Cfg.TestRepo.StagingRef, "refs/")
	prefixes := []struct {
		prefix string
		ids    map[int]bool
	}{
		{"heads/" + p.Cfg.TestRepo.BranchPrefix, expired},
		{staging + "series_", expired},
		{staging + "patch_", expiredPatches},
	}
	for _, pr := range prefixes {
		prefix := pr.prefix
		refs, err := p.GH.MatchingRefs(ctx, prefix)
		if err != nil {
			p.warnf("Cannot list refs under %s for cleanup: %v", prefix, err)
			return
		}
		for ref := range refs {
			name := strings.TrimPrefix(ref, "refs/")
			id, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
			if err != nil || !pr.ids[id] {
				continue
			}
			if deleted >= maxRefDeletes {
				p.infof("Stopping cleanup after %d deletions, the most per "+
					"invocation; the rest is deleted later.", deleted)
				return
			}
			if ctx.Err() != nil {
				p.infof("Stopping cleanup after %d deletions: %v.", deleted,
					ctx.Err())
				return
			}
			p.infof("Deleting old ref %s.", name)
			if err := p.GH.DeleteRef(ctx, name); err != nil {
				p.warnf("Could not delete old ref %s: %v", name, err)
				continue
			}
			deleted++
			p.infof("Deleted old ref %s.", name)
		}
	}
	p.infof("Ref cleanup finished: deleted %d refs.", deleted)
}
