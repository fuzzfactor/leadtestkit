// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"fmt"
	"testing"
	"time"
)

var t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

func limits() Limits {
	return Limits{MaxQueuedJobs: 100, MaxInflightPatches: 10, PerAuthorMin: 2,
		PerAuthorMaxShare: 0.5, JobsPerPatch: 1}
}

func TestSingleAuthorCappedByShare(t *testing.T) {
	got := Pick(Input{Limits: limits(), Candidates: []Candidate{
		{SeriesID: 1, Author: "lorem", Since: t0, Ready: 30},
	}})
	if len(got) != 5 {
		t.Errorf("picked %d, want 5 (50%% of 10)", len(got))
	}
}

func TestFairAcrossAuthors(t *testing.T) {
	got := Pick(Input{Limits: limits(), Candidates: []Candidate{
		{SeriesID: 1, Author: "lorem", Since: t0, Ready: 30},
		{SeriesID: 2, Author: "ipsum", Since: t0.Add(time.Hour), Ready: 1},
		{SeriesID: 3, Author: "dolor", Since: t0.Add(2 * time.Hour), Ready: 3},
	}})
	if fmt.Sprint(got) != "[1 2 3 1 3 1 3 1 1]" {
		t.Errorf("got %v", got)
	}
}

func TestInflightCounts(t *testing.T) {
	got := Pick(Input{Limits: limits(), Inflight: map[string]int{"lorem": 5, "ipsum": 4},
		Candidates: []Candidate{
			{SeriesID: 1, Author: "lorem", Since: t0, Ready: 3},
			{SeriesID: 2, Author: "ipsum", Since: t0, Ready: 3},
			{SeriesID: 3, Author: "dolor", Since: t0, Ready: 3},
		}})
	if fmt.Sprint(got) != "[3]" {
		t.Errorf("got %v", got)
	}
}

func TestQueuedJobsLimit(t *testing.T) {
	l := limits()
	l.MaxQueuedJobs = 20
	l.JobsPerPatch = 8
	got := Pick(Input{Limits: l, QueuedJobs: 5, Candidates: []Candidate{
		{SeriesID: 1, Author: "lorem", Since: t0, Ready: 5},
		{SeriesID: 2, Author: "ipsum", Since: t0, Ready: 5},
	}})
	if len(got) != 2 {
		t.Errorf("got %v", got)
	}
	if got := Pick(Input{Limits: l, QueuedJobs: 25, Candidates: []Candidate{
		{SeriesID: 1, Author: "lorem", Since: t0, Ready: 5}}}); len(got) != 0 {
		t.Errorf("full queue picked %v", got)
	}
}

func TestOldestSeriesOfAuthorFirst(t *testing.T) {
	got := Pick(Input{Limits: limits(), Candidates: []Candidate{
		{SeriesID: 9, Author: "lorem", Since: t0.Add(time.Hour), Ready: 1},
		{SeriesID: 4, Author: "lorem", Since: t0, Ready: 1},
	}})
	if fmt.Sprint(got) != "[4 9]" {
		t.Errorf("got %v", got)
	}
}

func TestPerAuthorMinAllowsSmallLimits(t *testing.T) {
	l := limits()
	l.MaxInflightPatches = 3
	l.PerAuthorMaxShare = 0.1
	got := Pick(Input{Limits: l, Candidates: []Candidate{
		{SeriesID: 1, Author: "lorem", Since: t0, Ready: 5}}})
	if len(got) != 2 {
		t.Errorf("got %v", got)
	}
}
