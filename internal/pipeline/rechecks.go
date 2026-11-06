// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"leadtestkit/internal/archive"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/state"
)

const commentEvents = "patch-comment-created"

var recheckRe = regexp.MustCompile(`(?m)^Recheck-request:[ \t]*(.+)$`)

func HasRecheck(content string, labels []string) bool {
	for _, m := range recheckRe.FindAllStringSubmatch(content, -1) {
		for _, tok := range strings.FieldsFunc(m[1], func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t'
		}) {
			for _, l := range labels {
				if strings.EqualFold(tok, l) {
					return true
				}
			}
		}
	}
	return false
}

type tracked struct {
	series *state.Series
	patch  *state.Patch
}

func (p *Pipeline) rechecks(ctx context.Context) {
	if !p.Cfg.Rechecks.Enabled {
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
		p.commentsFromEvents(ctx, patches)
	} else if p.Dev != nil {
		p.commentsFromArchive(ctx, patches)
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
		comments, err := p.PW.GetPatchComments(ctx, id)
		if err != nil {
			p.log().Warn("cannot fetch comments", "patch", id, "err", err)
			continue
		}
		t.patch.CommentsPending = false
		for _, c := range comments {
			if c.ID <= t.patch.CommentsSeen {
				continue
			}
			t.patch.CommentsSeen = c.ID
			if HasRecheck(c.Content, labels) {
				p.log().Info("recheck requested", "patch", id, "comment", c.ID)
				t.patch.Recheck = true
			}
		}
	}
	for _, id := range ids {
		if t := patches[id]; t.patch.Recheck && t.patch.Failure.Due(now) {
			p.doRecheck(ctx, t.series, t.patch)
		}
	}
}

func (p *Pipeline) commentsFromEvents(ctx context.Context, patches map[int]tracked) {
	opts := pw.WalkOptions{Category: commentEvents, Cursor: p.Store.Meta.Events[commentEvents]}
	if opts.Cursor == 0 {
		opts.Cutoff = p.now().Add(-24 * time.Hour)
		opts.MaxPages = 3
	}
	cursor, err := p.PW.WalkEvents(ctx, opts, func(ev pw.Event) error {
		if pl, ok := ev.Payload.(*pw.PatchCommentCreatedPayload); ok {
			if t, ok := patches[pl.Patch.ID]; ok {
				t.patch.CommentsPending = true
			}
		}
		return nil
	})
	if cursor > p.Store.Meta.Events[commentEvents] {
		p.Store.Meta.Events[commentEvents] = cursor
	}
	if err != nil {
		p.log().Warn("comment events", "err", err)
	}
}

// commentsFromArchive matches new dev list subjects against tracked
// patches, for Patchwork instances without comment events.
func (p *Pipeline) commentsFromArchive(ctx context.Context, patches map[int]tracked) {
	c := p.Store.Cursor(devCursor)
	months, next, ok := p.newMessages(ctx, p.Dev, c)
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
		return
	}
	names := map[int]string{}
	for id, t := range patches {
		names[id] = t.patch.Name
	}
	for _, id := range archive.MatchPatchSubjects(msgs, names) {
		patches[id].patch.CommentsPending = true
	}
}

func (p *Pipeline) doRecheck(ctx context.Context, s *state.Series, pt *state.Patch) {
	now := p.now()
	var rerun []*state.Run
	for _, r := range pt.Runs {
		if (r.Kind == state.RunPush || r.Kind == state.RunDispatch) && r.ID > 0 && r.Status == "completed" {
			rerun = append(rerun, r)
		}
	}
	if len(rerun) == 0 {
		p.log().Info("nothing to recheck", "patch", pt.ID)
		pt.Recheck = false
		return
	}
	if !p.Cfg.Mode.PushLive() {
		p.log().Info("would rerun workflows", "patch", pt.ID, "runs", len(rerun))
		pt.Recheck = false
		return
	}
	for _, r := range rerun {
		if err := p.GH.Rerun(ctx, r.ID, false); err != nil {
			if p.fail(&pt.Failure, "recheck", err) {
				pt.Recheck = false
				pt.Failure = nil
				p.errorf("patch %d: giving up rerunning workflows: %v", pt.ID, err)
			}
			return
		}
		r.MinAttempt = r.Attempt
		r.Status = "requested"
		r.Result, r.Conclusion, r.Completed, r.Details = "", "", "", ""
		r.Pending, r.Final = state.Report{}, state.Report{}
		r.AutoReruns = 0
		r.Updated = state.FormatTime(now)
	}
	var keep []*state.Run
	for _, r := range pt.Runs {
		if r.Kind != state.RunMissing {
			keep = append(keep, r)
		}
	}
	pt.Runs = keep
	pt.Failure = nil
	pt.Recheck = false
	pt.Status = state.PatchPushed
	pt.Pushed = state.FormatTime(now)
	if !s.Active() || s.Status != state.SeriesTesting {
		if p.Store.Find(s.ID) == nil {
			if err := p.Store.Reopen(s); err != nil {
				p.errorf("series %d: reopen: %v", s.ID, err)
			}
		}
		s.Status = state.SeriesTesting
		s.Reason = ""
		s.Finished = ""
	}
	p.Stats.Rechecks++
	p.log().Info("workflows rerun", "series", s.ID, "patch", pt.ID, "runs", len(rerun))
}
