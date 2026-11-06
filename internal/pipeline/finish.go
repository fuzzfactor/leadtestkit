// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"leadtestkit/internal/httpx"
	"leadtestkit/internal/state"
)

const maxRefDeletes = 50

// syncTools keeps the tools branch, the default branch of the test fork,
// close to upstream.  The resulting push runs the project's workflows
// there, which fills the caches that series branches can restore; runs
// only see caches of their own and of the default branch.
func (p *Pipeline) syncTools(ctx context.Context) {
	interval := p.Cfg.TestRepo.SyncInterval.D()
	if p.syncDone || !p.Cfg.Mode.PushLive() || interval <= 0 {
		return
	}
	p.syncDone = true
	now := p.now()
	if last := state.ParseTime(p.Store.Meta.ToolsSynced); !last.IsZero() && now.Sub(last) < interval {
		return
	}
	branch := p.Cfg.TestRepo.ToolsBranch
	kind, err := p.GH.MergeUpstream(ctx, branch)
	switch code := httpx.StatusCode(err); {
	case err == nil:
		p.log().Info("tools branch synced with upstream", "branch", branch, "merge", kind)
		p.customLoaded = false
	case code == http.StatusConflict || code == http.StatusUnprocessableEntity || code == http.StatusNotFound:
		// Retrying before the next interval would not help.
		p.errorf("cannot sync %s of the test fork with upstream (merge it manually): %v", branch, err)
	default:
		p.log().Warn("syncing the tools branch failed, will retry", "branch", branch, "err", err)
		return
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

func (p *Pipeline) finish(ctx context.Context) {
	now := p.now()
	for _, s := range append([]*state.Series(nil), p.Store.Queue...) {
		if s.Status == state.SeriesTesting {
			done := true
			for _, pt := range s.Patches {
				if !patchFinal(pt) {
					done = false
					break
				}
			}
			if done {
				s.Finish(state.SeriesDone, "", now)
				p.log().Info("series done", "series", s.ID)
			}
		}
		if s.Active() {
			continue
		}
		if s.Finished == "" {
			s.Finished = state.FormatTime(now)
		}
		if err := p.Store.Archive(s); err != nil {
			p.errorf("series %d: archive: %v", s.ID, err)
			continue
		}
		p.Stats.Finished++
	}
}

// gc only deletes refs of series known to have finished long ago; the
// fork may be shared with other tools.
func (p *Pipeline) gc(ctx context.Context) {
	if p.gcDone || !p.Cfg.Mode.PushLive() || p.Cfg.Limits.BranchRetention <= 0 {
		return
	}
	p.gcDone = true
	cutoff := p.now().Add(-p.Cfg.Limits.BranchRetention.D())
	months, err := p.Store.HistoryMonths()
	if err != nil {
		p.log().Warn("gc", "err", err)
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
			p.log().Warn("gc", "err", err)
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
			p.log().Warn("gc: cannot list refs", "err", err)
			return
		}
		for ref := range refs {
			name := strings.TrimPrefix(ref, "refs/")
			id, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
			if err != nil || !pr.ids[id] {
				continue
			}
			if deleted >= maxRefDeletes || ctx.Err() != nil {
				return
			}
			if err := p.GH.DeleteRef(ctx, name); err != nil {
				p.log().Warn("gc: cannot delete ref", "ref", name, "err", err)
				continue
			}
			deleted++
			p.log().Info("deleted old ref", "ref", name)
		}
	}
}
