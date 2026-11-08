// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
	"time"

	"leadtestkit/internal/logx"
	"leadtestkit/internal/state"
)

func (p *Pipeline) noteSeries(s *state.Series, action, format string, args ...any) {
	p.note(&s.Decisions, action, format, args...)
}

func (p *Pipeline) notePatch(pt *state.Patch, action, format string, args ...any) {
	p.note(&pt.Decisions, action, format, args...)
	p.Store.PatchChanged(pt)
}

// noteRun records a decision about an attempt of a workflow run and
// links it.
func (p *Pipeline) noteRun(pt *state.Patch, runURL string, attempt int, action, format string,
	args ...any) {
	p.note(&pt.Decisions, action, format, args...)
	if runURL != "" && attempt > 0 {
		runURL += fmt.Sprintf("/attempts/%d", attempt)
	}
	pt.Decisions[len(pt.Decisions)-1].WorkflowURL = runURL
	p.Store.PatchChanged(pt)
}

func (p *Pipeline) note(out *[]state.Decision, action, format string, args ...any) {
	d := state.Decision{
		Time: state.FormatTime(p.now()), Action: action,
		Detail: logx.Redact(fmt.Sprintf(format, args...)), RunURL: p.RunURL,
	}
	*out = append(*out, d)
}

func (p *Pipeline) finishSeries(s *state.Series, status, reason string, now time.Time) {
	var unfinished []*state.Patch
	for _, pt := range s.Patches {
		if pt.Unfinished() {
			unfinished = append(unfinished, pt)
		}
	}
	s.Finish(status, reason, now)
	detail := reason
	if detail == "" {
		if status == state.SeriesDone {
			detail = "All patches and reports are complete."
		} else {
			detail = "Series " + status + "."
		}
	}
	p.noteSeries(s, status, "%s", detail)
	for _, pt := range unfinished {
		if pt.Status == state.PatchSkipped {
			p.notePatch(pt, state.PatchSkipped, "Series %s: %s", status, detail)
		}
	}
}

func (p *Pipeline) noteCheck(patchID int, action, detail string) {
	recent, err := p.Store.Recent(p.now(), p.Cfg.Limits.HistoryWindow.D())
	if err != nil {
		p.errorf("patch %d: cannot record check decision: %v", patchID, err)
		return
	}
	_, pt := state.FindPatch(recent, patchID)
	if pt == nil {
		return
	}
	p.notePatch(pt, action, "%s", detail)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
