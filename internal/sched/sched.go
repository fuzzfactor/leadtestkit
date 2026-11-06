// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

// Package sched decides which patches to push next.
package sched

import (
	"math"
	"sort"
	"time"
)

type Limits struct {
	MaxQueuedJobs      int
	MaxInflightPatches int
	PerAuthorMin       int
	PerAuthorMaxShare  float64
	JobsPerPatch       int
}

type Candidate struct {
	SeriesID int
	Author   string
	Since    time.Time
	// Ready is the number of patches of the series that can be pushed.
	Ready int
}

type Input struct {
	Limits     Limits
	QueuedJobs int
	// Inflight counts pushed patches whose runs did not finish, per
	// author.
	Inflight   map[string]int
	Candidates []Candidate
}

// Pick returns series IDs in push order; a series appears once per
// patch to push.  Runners are kept busy as long as few jobs wait for a
// runner, which keeps the queue short so that a new author gets the
// next free slot.  The author with the fewest patches in flight goes
// first, and nobody may exceed a fair share of the in-flight limit.
func Pick(in Input) []int {
	l := in.Limits
	inflight := map[string]int{}
	total := 0
	for a, n := range in.Inflight {
		inflight[a] = n
		total += n
	}
	cands := append([]Candidate(nil), in.Candidates...)
	sort.SliceStable(cands, func(i, j int) bool {
		if !cands[i].Since.Equal(cands[j].Since) {
			return cands[i].Since.Before(cands[j].Since)
		}
		return cands[i].SeriesID < cands[j].SeriesID
	})

	maxShare := int(math.Floor(float64(l.MaxInflightPatches) * l.PerAuthorMaxShare))
	maxShare = max(maxShare, l.PerAuthorMin)

	queued := in.QueuedJobs
	var out []int
	for queued < l.MaxQueuedJobs && total < l.MaxInflightPatches {
		waiting := map[string]bool{}
		for _, c := range cands {
			if c.Ready > 0 {
				waiting[c.Author] = true
			}
		}
		if len(waiting) == 0 {
			break
		}
		fair := int(math.Ceil(float64(l.MaxInflightPatches) / float64(len(waiting))))
		limit := min(max(fair, l.PerAuthorMin), maxShare)

		best := -1
		for i, c := range cands {
			if c.Ready == 0 || inflight[c.Author] >= limit {
				continue
			}
			if best < 0 || inflight[c.Author] < inflight[cands[best].Author] {
				best = i
			}
		}
		if best < 0 {
			break
		}
		c := &cands[best]
		c.Ready--
		inflight[c.Author]++
		total++
		queued += l.JobsPerPatch
		out = append(out, c.SeriesID)
	}
	return out
}
