// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"strconv"
	"strings"
	"time"

	"leadtestkit/internal/state"
)

const maxRefDeletes = 50

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

// finish completes series whose patches are all reported and moves
// finished series to the history.
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

// gc deletes test branches and staging refs of series that finished
// longer ago than the retention period.  Refs of unknown series are
// left alone, since the fork may be shared with other tools.
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
			}
		}
	}
	for _, s := range p.Store.Queue {
		delete(expired, s.ID)
	}
	if len(expired) == 0 {
		return
	}
	deleted := 0
	prefixes := []string{
		"heads/" + p.Cfg.TestRepo.BranchPrefix,
		strings.TrimPrefix(p.Cfg.TestRepo.StagingRef, "refs/") + "series_",
	}
	for _, prefix := range prefixes {
		refs, err := p.GH.MatchingRefs(ctx, prefix)
		if err != nil {
			p.log().Warn("gc: cannot list refs", "err", err)
			return
		}
		for ref := range refs {
			name := strings.TrimPrefix(ref, "refs/")
			id, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
			if err != nil || !expired[id] {
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
