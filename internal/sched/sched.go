// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

// Package sched decides which patches to push next.
package sched

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
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
	Explain    func(format string, args ...any)
}

// Pick returns series IDs in push order; a series appears once per
// patch to push.  Runners are kept busy as long as few jobs wait for a
// runner, which keeps the queue short so that a new author gets the
// next free slot.  The author with the fewest patches in flight goes
// first, and nobody may exceed a fair share of the in-flight limit.
func Pick(in Input) []int {
	explain := func(format string, args ...any) {
		if in.Explain != nil {
			in.Explain(format, args...)
		}
	}
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
	// Authors at their limit, with the limit and the series that wait,
	// explained once after the selection.
	type block struct {
		limit  int
		series []int
	}
	blocked := map[string]*block{}
	for queued < l.MaxQueuedJobs && total < l.MaxInflightPatches {
		waiting := map[string]bool{}
		for _, c := range cands {
			if c.Ready > 0 {
				waiting[c.Author] = true
			}
		}
		if len(waiting) == 0 {
			explain("No more ready patches are available in this scheduling cycle.")
			break
		}
		fair := int(math.Ceil(float64(l.MaxInflightPatches) / float64(len(waiting))))
		limit := min(max(fair, l.PerAuthorMin), maxShare)

		best := -1
		for i, c := range cands {
			if c.Ready == 0 {
				continue
			}
			if inflight[c.Author] >= limit {
				b := blocked[c.Author]
				if b == nil {
					b = &block{}
					blocked[c.Author] = b
				}
				b.limit = limit
				if !slices.Contains(b.series, c.SeriesID) {
					b.series = append(b.series, c.SeriesID)
				}
				continue
			}
			if best < 0 || inflight[c.Author] < inflight[cands[best].Author] {
				best = i
			}
		}
		if best < 0 {
			explain("No author has a free slot under fairness limits; " +
				"not selecting more patches.")
			break
		}
		c := &cands[best]
		explain("Selected one patch from series %d for %s: %d patches in flight, "+
			"below the author's current limit of %d; older series win ties.",
			c.SeriesID, c.Author, inflight[c.Author], limit)
		c.Ready--
		inflight[c.Author]++
		total++
		queued += l.JobsPerPatch
		out = append(out, c.SeriesID)
	}
	authors := make([]string, 0, len(blocked))
	for a := range blocked {
		authors = append(authors, a)
	}
	sort.Strings(authors)
	for _, a := range authors {
		b := blocked[a]
		ids := make([]string, len(b.series))
		for i, id := range b.series {
			ids[i] = strconv.Itoa(id)
		}
		explain("%s has %d patches in flight, including this cycle's selections, "+
			"reaching the per-author limit of %d; series %s wait.",
			a, inflight[a], b.limit, strings.Join(ids, ", "))
	}
	selected := ""
	if len(out) > 0 {
		selected = fmt.Sprintf(", plus about %d for the %d patches selected now,",
			queued-in.QueuedJobs, len(out))
	}
	if queued >= l.MaxQueuedJobs {
		explain("Stopped selecting patches: %d jobs waiting for a runner on GitHub%s "+
			"reach the limit of %d.", in.QueuedJobs, selected, l.MaxQueuedJobs)
	}
	if total >= l.MaxInflightPatches {
		selected = ""
		if len(out) > 0 {
			selected = fmt.Sprintf(", plus %d selected now,", len(out))
		}
		explain("Stopped selecting patches: %d patches being tested%s reach the "+
			"limit of %d.", total-len(out), selected, l.MaxInflightPatches)
	}
	return out
}
