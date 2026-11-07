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
		p.infof("Not adding series %d: invalid or already tracked in the queue or history.",
			id)
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
	p.infof("Found series %d; fetching its details during eligibility checks.", id)
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
	meta := p.Store.Meta
	if meta.DiscoveryStart == "" {
		meta.DiscoveryStart = state.FormatTime(now)
		p.infof("First discovery: recording %s as the discovery start time.",
			meta.DiscoveryStart)
	}
	cutoff := now.Add(-p.Cfg.Limits.SeriesMaxAge.D())
	start := state.ParseTime(meta.DiscoveryStart)
	if c := start.Add(-p.Cfg.Limits.InitialBackfill.D()); c.After(cutoff) {
		cutoff = c
	}
	p.infof("Discovery start: %s. Initial backfill: %s. Maximum series age: %s.",
		meta.DiscoveryStart, p.Cfg.Limits.InitialBackfill.D(),
		p.Cfg.Limits.SeriesMaxAge.D())
	p.infof("The series-list cutoff is %s; older series will not be added from that list.",
		state.FormatTime(cutoff))

	opts := pw.WalkOptions{Category: seriesEvents, Cursor: meta.Events[seriesEvents]}
	if opts.Cursor == 0 {
		opts.Cutoff = cutoff
		opts.MaxPages = 3
		p.infof("No saved event cursor: scanning from %s, limited to %d event pages.",
			state.FormatTime(cutoff), opts.MaxPages)
		if p.Cfg.Limits.InitialBackfill.D() == 0 {
			p.infof("Initial backfill is zero: older series are not requested.")
		} else {
			p.infof("Backfill is bounded by the event-page limit and maximum age; " +
				"this is not an exhaustive historical scan.")
		}
	} else {
		p.infof("Resuming after series event %d. "+
			"Changing initial_backfill does not rewind this cursor; "+
			"only the newest-series safety scan uses the cutoff.",
			opts.Cursor)
	}
	before := p.Stats.Discovered
	events := 0
	cursor, err := p.PW.WalkEvents(ctx, opts, func(ev pw.Event) error {
		events++
		if pl, ok := ev.Payload.(*pw.SeriesCompletedPayload); ok {
			p.infof("Series-completed event %d: series %d (%s).",
				ev.ID, pl.Series.ID, ev.Date)
			p.track(pl.Series.ID, known)
		} else {
			p.infof("Ignoring series event %d: no series-completed payload.", ev.ID)
		}
		return nil
	})
	if cursor > meta.Events[seriesEvents] {
		meta.Events[seriesEvents] = cursor
	}
	if err != nil {
		p.warnf("Series-event scan failed: %v. Keeping progress through event %d.",
			err, meta.Events[seriesEvents])
	}
	p.infof("Read %d series events; added %d series. Saved event cursor: %d.",
		events, p.Stats.Discovered-before, meta.Events[seriesEvents])

	if p.safetyNetDone {
		p.infof("Not repeating the safety scan: already ran in this invocation.")
		return
	}
	p.infof("Safety scan: checking only the newest %d series for missing events.",
		safetyNetPageSize)
	page, err := p.PW.GetSeriesPage(ctx, p.PW.BuildSeriesURL(pw.SeriesListParams{
		Project: p.Cfg.Patchwork.Project, Order: "-id", PerPage: safetyNetPageSize,
	}))
	if err != nil {
		p.warnf("Cannot read the newest-series list; will retry in a later cycle: %v", err)
		return
	}
	p.safetyNetDone = true
	before = p.Stats.Discovered
	for _, s := range page.Items {
		if d := pw.ParseDate(s.Date); d.IsZero() || d.Before(cutoff) {
			p.infof("Series %d (%s): not adding from the safety scan; "+
				"date %q is missing "+
				"or before %s.", s.ID, s.Name, s.Date, state.FormatTime(cutoff))
			continue
		}
		p.track(s.ID, known)
	}
	p.infof("Safety scan finished: examined %d series and added %d.",
		len(page.Items), p.Stats.Discovered-before)
}

var terminalPatchStates = map[string]bool{
	"accepted": true, "rejected": true, "superseded": true, "changes-requested": true,
	"not-applicable": true, "handled-elsewhere": true, "deferred": true,
}

func (p *Pipeline) qualify(ctx context.Context) {
	now := p.now()
	for _, s := range p.Store.Queue {
		if ctx.Err() != nil {
			return
		}
		if s.Status != state.SeriesNew {
			continue
		}
		if s.Hold {
			p.infof("%s: eligibility checks paused by a manual hold.", seriesName(s))
			continue
		}
		if !s.Failure.Due(now) {
			p.infof("%s: waiting until %s to retry %s.",
				seriesName(s), s.Failure.Next, s.Failure.Op)
			continue
		}
		if c := state.ParseTime(s.Checked); !c.IsZero() && now.Sub(c) < incompleteRecheck {
			p.infof("%s: waiting for missing patches; next completeness check at %s.",
				seriesName(s), state.FormatTime(c.Add(incompleteRecheck)))
			continue
		}
		p.infof("Series %d: fetching details to check eligibility.", s.ID)
		ps, err := p.PW.GetSeries(ctx, s.ID)
		if err != nil {
			if p.fail(&s.Failure, "fetch series", err) {
				s.Finish(state.SeriesError,
					"fetch series: "+s.Failure.LastError, now)
				p.errorf("series %d: giving up fetching details: %v", s.ID, err)
			}
			continue
		}
		s.Failure = nil
		p.fill(s, ps)
		if reason := p.skipReason(s, ps, now); reason != "" {
			p.infof("%s: skipping: %s.", seriesName(s), reason)
			s.Finish(state.SeriesSkipped, reason, now)
			continue
		}
		if !ps.ReceivedAll {
			p.infof("%s: received %d of %d patches; waiting for the rest.",
				seriesName(s), len(ps.Patches), ps.Total)
			s.Checked = state.FormatTime(now)
			continue
		}
		s.Checked = ""
		s.Status = state.SeriesQueued
		p.infof("%s: eligible; queued %d patches for testing.",
			seriesName(s), len(s.Patches))
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
		discovered := state.ParseTime(s.Discovered)
		if now.Sub(discovered) > p.Cfg.Limits.CompleteTimeout.D() {
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

func (p *Pipeline) supersede(s *state.Series, now time.Time) {
	if !p.Cfg.Rules.Supersede || s.Version < 2 {
		return
	}
	name := baseName(s.Name)
	for _, o := range p.Store.Queue {
		if o == s || o.Version >= s.Version || !o.Active() ||
			(o.Status != state.SeriesQueued && o.Status != state.SeriesNew &&
				o.Status != state.SeriesWaiting) ||
			!strings.EqualFold(o.Submitter.Email, s.Submitter.Email) ||
			baseName(o.Name) != name {
			continue
		}
		p.infof("%s: skipping because series %d is a newer version.", seriesName(o), s.ID)
		o.Finish(state.SeriesSkipped, "superseded by a newer version", now)
	}
}
