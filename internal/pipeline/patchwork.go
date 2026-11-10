// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"net/http"

	"leadtestkit/internal/httpx"
	"leadtestkit/internal/pw"
)

// Every Patchwork request is retried for minutes before it fails, so
// once Patchwork does not answer, the rest of its work waits for the
// next polling cycle instead of failing request by request.

// pwUnavailable reports whether an error says that Patchwork does not
// work at all, rather than that one request is wrong.
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
	return "not asked: Patchwork failed earlier in this polling cycle: " + e.err.Error()
}

func (e *pwSkipError) Unwrap() error { return e.err }

// pwSkipped reports whether Patchwork failed in this polling cycle, and
// logs once per cycle what waits for the next one.
func (p *Pipeline) pwSkipped(what string) bool {
	if p.pwDown == nil {
		return false
	}
	if !p.pwSkipLogged[what] {
		if p.pwSkipLogged == nil {
			p.pwSkipLogged = map[string]bool{}
		}
		p.pwSkipLogged[what] = true
		p.infof("Not %s until the next polling cycle: Patchwork failed in this one: %v",
			what, p.pwDown)
	}
	return true
}

// pw is the Patchwork client as the stages use it.  Once a request
// fails because Patchwork does not work, the others of the polling
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
	if g.p.pwDown == nil && pwUnavailable(err) {
		g.p.pwDown = err
		g.p.warnf("Patchwork does not work; its other requests wait until the next "+
			"polling cycle: %v", err)
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
	return g.note(g.p.PW.CreateCheck(ctx, patchID, chk))
}

func (g pwGate) FindPatchesByMsgID(ctx context.Context, msgid string) ([]pw.Patch, error) {
	if err := g.skip(); err != nil {
		return nil, err
	}
	found, err := g.p.PW.FindPatchesByMsgID(ctx, msgid)
	return found, g.note(err)
}

func (g pwGate) PatchWebURL(id int) string { return g.p.PW.PatchWebURL(id) }
