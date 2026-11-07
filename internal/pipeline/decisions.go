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

func (p *Pipeline) note(out *[]state.Decision, action, format string, args ...any) {
	d := state.Decision{
		Time: state.FormatTime(p.now()), Action: action,
		Detail: logx.Redact(fmt.Sprintf(format, args...)), RunURL: p.RunURL,
	}
	*out = append(*out, d)
}

func (p *Pipeline) finishSeries(s *state.Series, status, reason string, now time.Time) {
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
