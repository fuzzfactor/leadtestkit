// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"net/http"
	"time"

	"leadtestkit/internal/httpx"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/state"
)

// Every Patchwork request is retried before it fails, so once Patchwork
// does not answer, the rest of its work waits for the next polling
// cycle instead of failing request by request.  The first request of
// every cycle checks whether it is available again.

// pwOutageAfter is how long Patchwork may not answer before it counts
// as an outage; it is known to be unreachable for a while at times.
const pwOutageAfter = 2 * time.Hour

// pwError is an error of a Patchwork request.
type pwError struct{ err error }

func (e *pwError) Error() string { return e.err.Error() }
func (e *pwError) Unwrap() error { return e.err }

func isPWError(err error) bool {
	var e *pwError
	return errors.As(err, &e)
}

// checksRefused reports Patchwork refusing to create a check, which it
// does when the user of the token is not a maintainer of the project.
func checksRefused(err error) bool {
	var se *httpx.StatusError
	return errors.As(err, &se) && se.Code == http.StatusForbidden && !se.RateLimited
}

// pwUnavailable reports whether an error says that Patchwork is
// unavailable, rather than that one request is wrong.
func pwUnavailable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *httpx.StatusError
	if !errors.As(err, &se) {
		// Network errors, bot protection and pages that are not the API.
		return true
	}
	// The token is sent with every request, so a refused one fails all.
	return se.Code == http.StatusUnauthorized || se.Code == http.StatusTooManyRequests ||
		se.Code >= 500 || se.RateLimited
}

// pwSkipError fails a Patchwork request that is not made because an
// earlier one failed in this polling cycle.
type pwSkipError struct{ err error }

func (e *pwSkipError) Error() string {
	return "not asked: Patchwork was unavailable earlier in this polling cycle: " +
		e.err.Error()
}

func (e *pwSkipError) Unwrap() error { return e.err }

// pwSkipped reports whether Patchwork was unavailable in this polling
// cycle, and logs once per cycle what waits for the next one.
func (p *Pipeline) pwSkipped(what string) bool {
	return p.pwSkip(what, p.pwDown, "Patchwork was unavailable in this one")
}

// checksSkipped is pwSkipped for work that posts checks, which also
// waits once Patchwork refused a check in this cycle.
func (p *Pipeline) checksSkipped(what string) bool {
	if p.pwSkipped(what) {
		return true
	}
	return p.pwSkip(what, p.checksDown, "Patchwork refused a check in this one")
}

func (p *Pipeline) pwSkip(what string, err error, why string) bool {
	if err == nil {
		return false
	}
	if !p.pwSkipLogged[what] {
		if p.pwSkipLogged == nil {
			p.pwSkipLogged = map[string]bool{}
		}
		p.pwSkipLogged[what] = true
		p.infof("Not %s until the next polling cycle: %s: %v", what, why, err)
	}
	return true
}

// pwWorked records a request that Patchwork answered.
func (p *Pipeline) pwWorked(check bool) {
	p.available(state.ServicePatchwork)
	if check {
		p.available(state.ServicePatchworkChecks)
	}
	o := p.outage(state.OutagePatchwork)
	switch {
	case o == nil:
	case check:
		p.endOutage(state.OutagePatchwork, "it accepted a check")
	case !o.Checks:
		p.endOutage(state.OutagePatchwork, "it answered a request")
	}
}

// pwFailed records a request that failed because Patchwork is
// unavailable.  A refused token does not get better by waiting.
func (p *Pipeline) pwFailed(err error) {
	now := p.now()
	per := p.unavailable(state.ServicePatchwork, err)
	since := state.ParseTime(per.Since)
	if httpx.StatusCode(err) == http.StatusUnauthorized {
		since = now
	} else if now.Sub(since) < pwOutageAfter {
		p.infof("Patchwork is unavailable since %s; that counts as an outage after %s.",
			per.Since, pwOutageAfter)
		return
	}
	if o := p.outage(state.OutagePatchwork); o != nil && o.Checks {
		// Refused checks were the visible part of a wider problem.
		o.Checks, o.Reason = false, err.Error()
		p.outageStarted(state.OutagePatchwork, o)
		p.errorf("%s: %s", p.describe(state.OutagePatchwork, o).what, o.Reason)
		return
	}
	p.startOutageWith(state.OutagePatchwork, &state.Outage{Reason: err.Error()}, since)
}

// pw is the Patchwork client as the stages use it.  Once a request
// fails because Patchwork is unavailable, the others of the polling
// cycle fail at once.
func (p *Pipeline) pw() pwGate { return pwGate{p} }

type pwGate struct{ p *Pipeline }

var _ Patchwork = pwGate{}

func (g pwGate) skip() error {
	if g.p.pwDown != nil {
		return &pwSkipError{g.p.pwDown}
	}
	return nil
}

func (g pwGate) note(err error) error {
	if err == nil {
		g.p.pwWorked(false)
		return nil
	}
	err = &pwError{err}
	if g.p.pwDown == nil && pwUnavailable(err) {
		g.p.pwDown = err
		g.p.warnf("Patchwork is unavailable; its other requests wait until the next "+
			"polling cycle: %v", err)
		g.p.pwFailed(err)
	}
	return err
}

func (g pwGate) WalkEvents(ctx context.Context, opts pw.WalkOptions,
	handle func(pw.Event) error) (int, error) {
	if err := g.skip(); err != nil {
		return opts.Cursor, err
	}
	cursor, err := g.p.PW.WalkEvents(ctx, opts, handle)
	return cursor, g.note(err)
}

func (g pwGate) BuildSeriesURL(params pw.SeriesListParams) string {
	return g.p.PW.BuildSeriesURL(params)
}

func (g pwGate) GetSeriesPage(ctx context.Context,
	pageURL string) (*pw.PageResult[pw.Series], error) {
	if err := g.skip(); err != nil {
		return nil, err
	}
	page, err := g.p.PW.GetSeriesPage(ctx, pageURL)
	return page, g.note(err)
}

func (g pwGate) GetSeries(ctx context.Context, id int) (*pw.Series, error) {
	if err := g.skip(); err != nil {
		return nil, err
	}
	s, err := g.p.PW.GetSeries(ctx, id)
	return s, g.note(err)
}

func (g pwGate) GetPatch(ctx context.Context, id int) (*pw.PatchDetail, error) {
	if err := g.skip(); err != nil {
		return nil, err
	}
	d, err := g.p.PW.GetPatch(ctx, id)
	return d, g.note(err)
}

func (g pwGate) GetCover(ctx context.Context, id int) (*pw.CoverDetail, error) {
	if err := g.skip(); err != nil {
		return nil, err
	}
	c, err := g.p.PW.GetCover(ctx, id)
	return c, g.note(err)
}

func (g pwGate) GetPatchComments(ctx context.Context, id int) ([]pw.Comment, error) {
	if err := g.skip(); err != nil {
		return nil, err
	}
	c, err := g.p.PW.GetPatchComments(ctx, id)
	return c, g.note(err)
}

func (g pwGate) GetPatchChecks(ctx context.Context, id int) ([]pw.Check, error) {
	if err := g.skip(); err != nil {
		return nil, err
	}
	c, err := g.p.PW.GetPatchChecks(ctx, id)
	return c, g.note(err)
}

func (g pwGate) CreateCheck(ctx context.Context, patchID int, chk pw.NewCheck) error {
	if err := g.skip(); err != nil {
		return err
	}
	if g.p.checksDown != nil {
		return &pwSkipError{g.p.checksDown}
	}
	err := g.p.PW.CreateCheck(ctx, patchID, chk)
	if err == nil {
		g.p.pwWorked(true)
		return nil
	}
	if !checksRefused(err) {
		return g.note(err)
	}
	err = &pwError{err}
	g.p.checksDown = err
	g.p.warnf("Patchwork refuses checks; the other checks wait until the next polling "+
		"cycle: %v", err)
	// The request was answered, so Patchwork is available otherwise.
	g.p.available(state.ServicePatchwork)
	g.p.unavailable(state.ServicePatchworkChecks, err)
	if g.p.outage(state.OutagePatchwork) == nil {
		g.p.startOutageWith(state.OutagePatchwork,
			&state.Outage{Reason: err.Error(), Checks: true}, g.p.now())
	}
	return err
}

func (g pwGate) FindPatchesByMsgID(ctx context.Context, msgid string) ([]pw.Patch, error) {
	if err := g.skip(); err != nil {
		return nil, err
	}
	found, err := g.p.PW.FindPatchesByMsgID(ctx, msgid)
	return found, g.note(err)
}

func (g pwGate) PatchWebURL(id int) string { return g.p.PW.PatchWebURL(id) }
