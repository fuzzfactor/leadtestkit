// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"leadtestkit/internal/pw"
	"leadtestkit/internal/state"
)

const (
	seriesEvents      = "series-completed"
	safetyNetPageSize = 50
	incompleteRecheck = 30 * time.Minute
)

func (p *Pipeline) track(id int, known map[int]bool) {
	if id <= 0 || known[id] {
		return
	}
	known[id] = true
	p.Store.Add(&state.Series{
		ID:         id,
		Status:     state.SeriesNew,
		Discovered: state.FormatTime(p.now()),
		Patches:    []*state.Patch{},
	})
	p.Stats.Discovered++
	p.log().Info("new series", "series", id)
}

// discover finds completed series through events, and once per run
// through the newest series list, since some instances drop events.
func (p *Pipeline) discover(ctx context.Context) {
	known, err := p.known()
	if err != nil {
		p.errorf("discover: %v", err)
		return
	}
	now := p.now()
	opts := pw.WalkOptions{Category: seriesEvents, Cursor: p.Store.Meta.Events[seriesEvents]}
	if opts.Cursor == 0 {
		opts.Cutoff = now.Add(-p.Cfg.Limits.SeriesMaxAge.D())
		opts.MaxPages = 3
	}
	cursor, err := p.PW.WalkEvents(ctx, opts, func(ev pw.Event) error {
		if pl, ok := ev.Payload.(*pw.SeriesCompletedPayload); ok {
			p.track(pl.Series.ID, known)
		}
		return nil
	})
	if cursor > p.Store.Meta.Events[seriesEvents] {
		p.Store.Meta.Events[seriesEvents] = cursor
	}
	if err != nil {
		p.log().Warn("series events", "err", err)
	}

	if p.safetyNetDone {
		return
	}
	page, err := p.PW.GetSeriesPage(ctx, p.PW.BuildSeriesURL(pw.SeriesListParams{
		Project: p.Cfg.Patchwork.Project, Order: "-id", PerPage: safetyNetPageSize,
	}))
	if err != nil {
		p.log().Warn("series list", "err", err)
		return
	}
	p.safetyNetDone = true
	cutoff := now.Add(-p.Cfg.Limits.SeriesMaxAge.D())
	for _, s := range page.Items {
		if d := pw.ParseDate(s.Date); !d.IsZero() && d.Before(cutoff) {
			continue
		}
		p.track(s.ID, known)
	}
}

var terminalPatchStates = map[string]bool{
	"accepted": true, "rejected": true, "superseded": true, "changes-requested": true,
	"not-applicable": true, "handled-elsewhere": true, "deferred": true,
}

// qualify fetches details of new series and decides whether to test them.
func (p *Pipeline) qualify(ctx context.Context) {
	now := p.now()
	for _, s := range p.Store.Queue {
		if ctx.Err() != nil {
			return
		}
		if s.Status != state.SeriesNew || s.Hold || !s.Failure.Due(now) {
			continue
		}
		if c := state.ParseTime(s.Checked); !c.IsZero() && now.Sub(c) < incompleteRecheck {
			continue
		}
		ps, err := p.PW.GetSeries(ctx, s.ID)
		if err != nil {
			if p.fail(&s.Failure, "fetch series", err) {
				s.Finish(state.SeriesError, "fetch series: "+s.Failure.LastError, now)
				p.errorf("series %d: giving up fetching details: %v", s.ID, err)
			}
			continue
		}
		s.Failure = nil
		p.fill(s, ps)
		if reason := p.skipReason(s, ps, now); reason != "" {
			p.log().Info("series skipped", "series", s.ID, "reason", reason)
			s.Finish(state.SeriesSkipped, reason, now)
			continue
		}
		if !ps.ReceivedAll {
			s.Checked = state.FormatTime(now)
			continue
		}
		s.Checked = ""
		s.Status = state.SeriesQueued
		p.log().Info("series queued", "series", s.ID, "name", s.Name, "patches", len(s.Patches))
		p.supersede(s, now)
	}
}

func (p *Pipeline) fill(s *state.Series, ps *pw.Series) {
	s.Name = ps.Name
	s.Version = ps.Version
	s.Date = ps.Date
	s.WebURL = ps.WebURL
	s.Submitter = state.Person{Name: ps.Submitter.Name, Email: ps.Submitter.Email}
	s.Total = ps.Total
	if ps.CoverLetter != nil {
		s.CoverID = ps.CoverLetter.ID
	}
	if !ps.ReceivedAll {
		return
	}
	summaries := append([]pw.PatchSummary(nil), ps.Patches...)
	sort.SliceStable(summaries, func(i, j int) bool {
		return pw.PatchNumber(summaries[i].Name) < pw.PatchNumber(summaries[j].Name)
	})
	existing := map[int]*state.Patch{}
	for _, pt := range s.Patches {
		existing[pt.ID] = pt
	}
	patches := make([]*state.Patch, 0, len(summaries))
	for i, sum := range summaries {
		pt := existing[sum.ID]
		if pt == nil {
			pt = &state.Patch{ID: sum.ID, Status: state.PatchPending}
		}
		pt.Number = i + 1
		pt.Name = sum.Name
		pt.MsgID = sum.MsgID
		patches = append(patches, pt)
	}
	s.Patches = patches
}

func (p *Pipeline) skipReason(s *state.Series, ps *pw.Series, now time.Time) string {
	if d := pw.ParseDate(ps.Date); !d.IsZero() && now.Sub(d) > p.Cfg.Limits.SeriesMaxAge.D() {
		return "older than series_max_age"
	}
	if !ps.ReceivedAll {
		if d := state.ParseTime(s.Discovered); now.Sub(d) > p.Cfg.Limits.CompleteTimeout.D() {
			return "series never completed"
		}
		return ""
	}
	if len(ps.Patches) == 0 {
		return "no patches"
	}
	if max := p.Cfg.Limits.MaxSeriesPatches; max > 0 && len(ps.Patches) > max {
		return "too many patches"
	}
	names := []string{ps.Name}
	for _, pt := range ps.Patches {
		names = append(names, pt.Name)
	}
	for i := range p.Cfg.Rules.Skip {
		r := &p.Cfg.Rules.Skip[i]
		for _, n := range names {
			if r.Regexp().MatchString(n) {
				reason := r.Reason
				if reason == "" {
					reason = "matches " + r.Subject
				}
				return reason
			}
		}
	}
	return ""
}

var versionTagRe = regexp.MustCompile(`(?i)^\s*(\[[^\]]*\]\s*)+`)

func baseName(name string) string {
	return strings.ToLower(strings.TrimSpace(versionTagRe.ReplaceAllString(name, "")))
}

// supersede skips older versions of a series that were not started.
func (p *Pipeline) supersede(s *state.Series, now time.Time) {
	if !p.Cfg.Rules.Supersede || s.Version < 2 {
		return
	}
	name := baseName(s.Name)
	for _, o := range p.Store.Queue {
		if o == s || o.Version >= s.Version || !o.Active() ||
			(o.Status != state.SeriesQueued && o.Status != state.SeriesNew &&
				o.Status != state.SeriesWaiting) ||
			!strings.EqualFold(o.Submitter.Email, s.Submitter.Email) || baseName(o.Name) != name {
			continue
		}
		p.log().Info("series superseded", "series", o.ID, "by", s.ID)
		o.Finish(state.SeriesSkipped, "superseded by a newer version", now)
	}
}
