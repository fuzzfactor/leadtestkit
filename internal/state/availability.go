// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package state

import "time"

const AvailabilityFile = "availability.json"

// Services whose availability is recorded.  Archives use OutageArchive.
const (
	ServicePatchwork = "patchwork"
	// ServicePatchworkChecks: Patchwork refuses the checks of the bot.
	ServicePatchworkChecks = "patchwork-checks"
	// ServiceGitHub: the GitHub API, as the GitHub App of the bot.
	ServiceGitHub = "github"
	// ServiceWorkflows: workflows run on the test repository.
	ServiceWorkflows = "workflows"
	ServiceEmail     = "email"
)

// Availability records when the services the bot uses were unavailable,
// as the bot saw it: from the first failed request to the first that
// worked.  Short periods that never became an outage count too.
type Availability struct {
	Schema int `json:"schema"`
	// Since is when the bot started to record availability; nothing is
	// known before.
	Since string `json:"since,omitempty"`
	// Failing holds the periods that have not ended, by service.
	Failing map[string]*Period `json:"failing"`
	// Periods are the ended periods, oldest first.
	Periods []*Period `json:"periods"`
}

type Period struct {
	Service string `json:"service"`
	Since   string `json:"since"`
	Until   string `json:"until,omitempty"`
	Reason  string `json:"reason"`
	// Outage is set when the bot declared an outage during the period.
	Outage bool `json:"outage,omitempty"`
}

func newAvailability() *Availability {
	return &Availability{Schema: SchemaVersion, Failing: map[string]*Period{}}
}

// Unavailable starts a period of a service unless one is going on, and
// returns the period.
func (a *Availability) Unavailable(service, reason string, now time.Time) *Period {
	if p := a.Failing[service]; p != nil {
		return p
	}
	p := &Period{Service: service, Since: FormatTime(now), Reason: reason}
	a.Failing[service] = p
	return p
}

// Available ends the period of a service, if any, and returns it.
func (a *Availability) Available(service string, now time.Time) *Period {
	p := a.Failing[service]
	if p == nil {
		return nil
	}
	delete(a.Failing, service)
	p.Until = FormatTime(now)
	a.Periods = append(a.Periods, p)
	return p
}

// Prune forgets the periods that ended before a time.
func (a *Availability) Prune(before time.Time) {
	keep := a.Periods[:0]
	for _, p := range a.Periods {
		if !ParseTime(p.Until).Before(before) {
			keep = append(keep, p)
		}
	}
	a.Periods = keep
}
