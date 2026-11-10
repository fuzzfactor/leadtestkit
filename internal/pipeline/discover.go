// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"leadtestkit/internal/config"
	"leadtestkit/internal/maint"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/state"
)

const (
	seriesEvents      = "series-completed"
	safetyNetPageSize = 50
	incompleteRecheck = 30 * time.Minute
)

// seen counts the series a discovery pass found, for its summary.
type seen struct {
	added, queued, history []int
}

func (n *seen) String() string {
	if parts := n.parts(); len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	return "none"
}

func (n *seen) parts() []string {
	var parts []string
	for _, x := range []struct {
		what string
		ids  []int
	}{{"added", n.added}, {"already in the queue", n.queued},
		{"already in the history", n.history}} {
		if len(x.ids) > 0 {
			parts = append(parts, fmt.Sprintf("%d %s (%s)", len(x.ids), x.what,
				joinInts(x.ids, 10)))
		}
	}
	return parts
}

// joinInts lists up to max numbers.
func joinInts(ids []int, max int) string {
	list := make([]string, len(ids))
	for i, id := range ids {
		list[i] = strconv.Itoa(id)
	}
	return joinList(list, max)
}

// joinList lists up to max items and counts the rest, to keep long
// lists of series readable in the log.
func joinList(items []string, max int) string {
	if len(items) > max {
		items = append(items[:max:max], fmt.Sprintf("and %d more", len(items)-max))
	}
	return strings.Join(items, ", ")
}

// track adds a series found in Patchwork, whose name is given if known.
func (p *Pipeline) track(id int, name string, known map[int]bool, n *seen) {
	switch {
	case id <= 0:
		p.warnf("Ignoring series ID %d from Patchwork: not a valid ID.", id)
		return
	case known[id]:
		if p.Store.Find(id) != nil {
			n.queued = append(n.queued, id)
		} else {
			n.history = append(n.history, id)
		}
		return
	}
	n.added = append(n.added, id)
	known[id] = true
	s := &state.Series{
		ID:         id,
		Status:     state.SeriesNew,
		Discovered: state.FormatTime(p.now()),
		Patches:    []*state.Patch{},
	}
	p.noteSeries(s, "discovered", "Found in Patchwork.")
	p.Store.Add(s)
	p.Stats.Discovered++
	if name != "" {
		p.infof("Found series %d (%s); checking it for eligibility next.", id, name)
	} else {
		p.infof("Found series %d; checking it for eligibility next.", id)
	}
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
	var fromEvents seen
	events := 0
	cursor, err := p.pw().WalkEvents(ctx, opts, func(ev pw.Event) error {
		events++
		if pl, ok := ev.Payload.(*pw.SeriesCompletedPayload); ok {
			p.track(pl.Series.ID, pl.Series.Name, known, &fromEvents)
		} else {
			p.warnf("Ignoring series event %d: no series-completed payload.", ev.ID)
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
	p.infof("Read %d series events: %s. Saved event cursor: %d.",
		events, &fromEvents, meta.Events[seriesEvents])

	if p.safetyNetDone {
		p.infof("Not repeating the safety scan: already ran in this invocation.")
		return
	}
	if p.pwSkipped("running the safety scan of the newest series") {
		return
	}
	p.infof("Safety scan: checking only the newest %d series for missing events.",
		safetyNetPageSize)
	page, err := p.pw().GetSeriesPage(ctx, p.pw().BuildSeriesURL(pw.SeriesListParams{
		Project: p.Cfg.Patchwork.Project, Order: "-id", PerPage: safetyNetPageSize,
	}))
	if err != nil {
		p.warnf("Cannot read the newest-series list; will retry in a later cycle: %v", err)
		return
	}
	p.safetyNetDone = true
	var fromList seen
	var old []int
	for _, s := range page.Items {
		d := pw.ParseDate(s.Date)
		if d.IsZero() {
			p.warnf("Series %d (%s): not adding from the safety scan; "+
				"Patchwork gives no valid date (%q).", s.ID, s.Name, s.Date)
			continue
		}
		if d.Before(cutoff) {
			old = append(old, s.ID)
			continue
		}
		p.track(s.ID, s.Name, known, &fromList)
	}
	parts := fromList.parts()
	if len(old) > 0 {
		parts = append(parts, fmt.Sprintf("%d from before the cutoff %s, not added (%s)",
			len(old), state.FormatTime(cutoff), joinInts(old, 10)))
	}
	if len(parts) == 0 {
		parts = append(parts, "none")
	}
	p.infof("Safety scan of the newest %d series: %s.", len(page.Items),
		strings.Join(parts, "; "))
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
		if p.pwSkipped("checking new series") {
			return
		}
		p.infof("Series %d: fetching details to check eligibility.", s.ID)
		ps, err := p.pw().GetSeries(ctx, s.ID)
		if err != nil {
			if p.fail(&s.Failure, "fetch series", err) {
				p.finishSeries(s, state.SeriesError,
					"fetch series: "+s.Failure.LastError, now)
				p.errorf("series %d: giving up fetching details: %v", s.ID, err)
			}
			p.noteSeries(s, "fetch-failed", "Attempt %d: %s. Next retry: %s.",
				s.Failure.Attempts, s.Failure.LastError, s.Failure.Next)
			continue
		}
		s.Failure = nil
		p.fill(s, ps)
		if reason := p.skipReason(s, ps, now); reason != "" {
			p.infof("%s: skipping: %s.", seriesName(s), reason)
			p.finishSeries(s, state.SeriesSkipped, reason, now)
			continue
		}
		if !ps.ReceivedAll {
			if s.Checked == "" {
				p.noteSeries(s, "incomplete", "Waiting for all %d patches.",
					ps.Total)
			}
			p.infof("%s: received %d of %d patches; waiting for the rest.",
				seriesName(s), len(ps.Patches), ps.Total)
			s.Checked = state.FormatTime(now)
			continue
		}
		s.Checked = ""
		s.Status = state.SeriesQueued
		p.noteSeries(s, "queued", "Eligible for testing; %d patches.", len(s.Patches))
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
			p.notePatch(pt, "discovered", "Found in series %d.", s.ID)
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
	// Rules on files need the diffs and are checked in prepare.
	for i := range p.Cfg.Rules.Skip {
		r := &p.Cfg.Rules.Skip[i]
		if r.Files != "" {
			continue
		}
		if n := firstMatch(r.Regexp(), names); n != "" {
			p.infof("Series %d: %q matches skip rule %q.", s.ID, n, r.Subject)
			return skipRuleReason(r, "matches "+r.Subject)
		}
	}
	return ""
}

// fileSkipReason applies the skip rules on files to the fetched patches.
func (p *Pipeline) fileSkipReason(s *state.Series, patches []*state.Patch,
	data []patchData) string {
	names := []string{s.Name}
	for _, pt := range patches {
		names = append(names, pt.Name)
	}
	for i := range p.Cfg.Rules.Skip {
		r := &p.Cfg.Rules.Skip[i]
		if r.Files == "" {
			continue
		}
		if r.Subject != "" && firstMatch(r.Regexp(), names) == "" {
			continue
		}
		for j, d := range data {
			files := maint.DiffFiles(d.detail.Diff)
			if f := firstMatch(r.FilesRegexp(), files); f != "" {
				p.infof("%s: changes %s, which matches skip rule %q.",
					patchName(patches[j]), f, r.Files)
				return skipRuleReason(r, "changes "+f)
			}
		}
	}
	return ""
}

func firstMatch(re *regexp.Regexp, list []string) string {
	for _, x := range list {
		if re.MatchString(x) {
			return x
		}
	}
	return ""
}

func skipRuleReason(r *config.SkipRule, fallback string) string {
	if r.Reason != "" {
		return r.Reason
	}
	return fallback
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
		p.finishSeries(o, state.SeriesSkipped,
			"superseded by a newer version", now)
	}
}
