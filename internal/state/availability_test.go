// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"testing"
	"time"
)

func TestAvailability(t *testing.T) {
	dir := t.TempDir()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	a := s.Availability
	p := a.Unavailable(ServicePatchwork, "lorem", t0)
	if again := a.Unavailable(ServicePatchwork, "ipsum", t0.Add(time.Minute)); again != p ||
		p.Reason != "lorem" {
		t.Fatalf("second failure started a new period: %+v", again)
	}
	if a.Available(ServiceGitHub, t0) != nil {
		t.Fatal("ended a period that did not start")
	}
	a.Available(ServicePatchwork, t0.Add(10*time.Minute))
	a.Unavailable(ServiceEmail, "dolor", t0.Add(time.Hour))
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s, err = Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	a = s.Availability
	if len(a.Periods) != 1 || a.Periods[0].Until != "2026-03-10T12:10:00Z" ||
		a.Failing[ServiceEmail] == nil {
		t.Fatalf("availability = %+v", a)
	}
	a.Prune(t0.Add(11 * time.Minute))
	if len(a.Periods) != 0 || a.Failing[ServiceEmail] == nil {
		t.Fatalf("after pruning: %+v", a)
	}
}

func TestDowntime(t *testing.T) {
	t0 := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	a := newAvailability()
	// Down 11:50-12:10, 12:30-12:40 and 13:00-13:20, and email at 12:30-13:30.
	a.Unavailable(ServicePatchwork, "lorem", t0.Add(-10*time.Minute))
	a.Available(ServicePatchwork, t0.Add(10*time.Minute))
	a.Unavailable(ServiceEmail, "ipsum", t0.Add(30*time.Minute))
	a.Unavailable(ServicePatchwork, "dolor", t0.Add(30*time.Minute))
	a.Available(ServicePatchwork, t0.Add(40*time.Minute))
	a.Available(ServiceEmail, t0.Add(90*time.Minute))
	a.Unavailable(ServicePatchwork, "sit", t0.Add(time.Hour))

	for _, tc := range []struct {
		from, to time.Duration
		want     time.Duration
	}{
		// Periods are cut at both ends; the period going on lasts
		// until the end.
		{0, 75 * time.Minute, 35 * time.Minute},
		{15 * time.Minute, 25 * time.Minute, 0},
		{35 * time.Minute, 2 * time.Hour, 65 * time.Minute},
		{-time.Hour, -30 * time.Minute, 0},
	} {
		got := a.Downtime(ServicePatchwork, t0.Add(tc.from), t0.Add(tc.to))
		if got != tc.want {
			t.Errorf("downtime from %s to %s = %s, want %s", tc.from, tc.to, got, tc.want)
		}
	}
}
