// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"leadtestkit/internal/httpx"
	"leadtestkit/internal/state"
)

// Once Patchwork fails in a polling cycle, its other requests wait for
// the next cycle, and the reports archive is not used up meanwhile.
func TestPatchworkFailureSkipsRestOfCycle(t *testing.T) {
	e := newEnv(t, "")
	e.pw.addSeries(80, 1, "lorem", e.now)
	e.pw.addSeries(81, 1, "ipsum", e.now)
	e.pw.fail["series"] = 1
	e.step()
	if e.pw.calls["series"] != 1 || e.p.reportsScanned {
		t.Fatalf("series requests = %d, reports scanned = %v", e.pw.calls["series"],
			e.p.reportsScanned)
	}
	if s := e.series(81); s.Failure != nil || s.Status != state.SeriesNew {
		t.Fatalf("skipped series = %+v", s)
	}
	e.advance(6 * time.Minute)
	e.step()
	if e.pw.calls["series"] != 3 || !e.p.reportsScanned {
		t.Fatalf("next cycle: series requests = %d, reports scanned = %v",
			e.pw.calls["series"], e.p.reportsScanned)
	}
}

func TestPWUnavailable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errors.New("dial tcp: connection refused"), true},
		{fmt.Errorf("%w: lorem", httpx.ErrBotProtection), true},
		{&httpx.StatusError{Code: 503}, true},
		{&httpx.StatusError{Code: 429}, true},
		{&httpx.StatusError{Code: 401}, true},
		{&httpx.StatusError{Code: 404}, false},
		{&httpx.StatusError{Code: 403}, false},
		{&httpx.StatusError{Code: 400}, false},
		{fmt.Errorf("lorem: %w", context.DeadlineExceeded), true},
		{context.Canceled, false},
		{nil, false},
	} {
		if got := pwUnavailable(tc.err); got != tc.want {
			t.Errorf("%v: got %v, want %v", tc.err, got, tc.want)
		}
	}
}

// Requested rechecks only need GitHub, so they do not wait for
// Patchwork.
func TestRecheckWithoutPatchwork(t *testing.T) {
	e := newEnv(t, "")
	_, failed := testedPatch(t, e, 240)
	pt := e.series(240).Patches[0]
	pt.Recheck, pt.CommentsPending = true, true
	e.pw.errs["*"] = errTransient
	e.step()
	if fmt.Sprint(e.gh.reruns) != fmt.Sprint([]int64{failed}) || !pt.CommentsPending {
		t.Fatalf("reruns = %v, patch = %+v", e.gh.reruns, pt)
	}
}

// A request that times out counts, but not one of a stopped cycle.
func TestPatchworkTimeouts(t *testing.T) {
	e := newEnv(t, "")
	e.pw.errs["series"] = fmt.Errorf("lorem (Client.Timeout exceeded): %w",
		context.DeadlineExceeded)
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.p.pw().GetSeries(stopped, 1); err == nil || e.p.pwDown != nil ||
		e.p.Store.Availability.Failing[state.ServicePatchwork] != nil {
		t.Fatalf("a stopped cycle counted: %v, %v", err, e.p.pwDown)
	}
	if !ghUnavailable(e.pw.errs["series"]) {
		t.Error("a timeout of GitHub does not count")
	}
	if _, err := e.p.pw().GetSeries(context.Background(), 1); err == nil ||
		e.p.pwDown == nil {
		t.Fatalf("a timeout did not count: %v", err)
	}
}
