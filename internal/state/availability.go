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
	// PatchworkHourlyTries counts the HTTP tries to Patchwork by hour for
	// the last days, oldest first.
	PatchworkHourlyTries []*HourTries `json:"patchwork_hourly_tries,omitempty"`
	// PatchworkDailyTries counts them by UTC day before that, oldest
	// first.
	PatchworkDailyTries []*DayTries `json:"patchwork_daily_tries,omitempty"`
}

type Period struct {
	Service string `json:"service"`
	Since   string `json:"since"`
	Until   string `json:"until,omitempty"`
	Reason  string `json:"reason"`
	// Outage is set when the bot declared an outage during the period.
	Outage bool `json:"outage,omitempty"`
}

// HourTries counts the tries of requests during an hour, each retry a
// try of its own.
type HourTries struct {
	// Hour is the start of the hour, in UTC.
	Hour   string `json:"hour"`
	Tries  int    `json:"tries"`
	Failed int    `json:"failed,omitempty"`
}

// DayTries counts the tries of requests during a UTC day.
type DayTries struct {
	// Day is the date, as 2006-01-02.
	Day    string `json:"day"`
	Tries  int    `json:"tries"`
	Failed int    `json:"failed,omitempty"`
}

const dayFormat = "2006-01-02"

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

// CountPatchworkTry counts a try of a request to Patchwork.
func (a *Availability) CountPatchworkTry(failed bool, now time.Time) {
	hour := FormatTime(now.UTC().Truncate(time.Hour))
	n := len(a.PatchworkHourlyTries)
	if n == 0 || a.PatchworkHourlyTries[n-1].Hour != hour {
		a.PatchworkHourlyTries = append(a.PatchworkHourlyTries, &HourTries{Hour: hour})
		n++
	}
	t := a.PatchworkHourlyTries[n-1]
	t.Tries++
	if failed {
		t.Failed++
	}
}

// PrunePatchworkTries merges the hours of each UTC day that ended more
// than hourly before now into a count for the day, and forgets the
// counts older than kept.
func (a *Availability) PrunePatchworkTries(now time.Time, hourly, kept time.Duration) {
	merged, forgotten := now.Add(-hourly), now.Add(-kept)
	hours := a.PatchworkHourlyTries[:0]
	for _, t := range a.PatchworkHourlyTries {
		start := ParseTime(t.Hour).UTC()
		if start.Before(forgotten) {
			continue
		}
		day := start.Truncate(24 * time.Hour)
		if day.Add(24 * time.Hour).After(merged) {
			hours = append(hours, t)
			continue
		}
		key := day.Format(dayFormat)
		n := len(a.PatchworkDailyTries)
		if n == 0 || a.PatchworkDailyTries[n-1].Day != key {
			a.PatchworkDailyTries = append(a.PatchworkDailyTries, &DayTries{Day: key})
			n++
		}
		d := a.PatchworkDailyTries[n-1]
		d.Tries += t.Tries
		d.Failed += t.Failed
	}
	a.PatchworkHourlyTries = hours
	days := a.PatchworkDailyTries[:0]
	for _, d := range a.PatchworkDailyTries {
		start, err := time.Parse(dayFormat, d.Day)
		if err == nil && start.Add(24*time.Hour).After(forgotten) {
			days = append(days, d)
		}
	}
	a.PatchworkDailyTries = days
}
