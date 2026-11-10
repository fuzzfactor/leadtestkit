// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"net/http"
	"time"

	"leadtestkit/internal/gh"
	"leadtestkit/internal/ghapp"
	"leadtestkit/internal/httpx"
	"leadtestkit/internal/state"
)

// availabilityKept is how long ended periods of unavailability are
// kept for the pages.
const availabilityKept = 90 * 24 * time.Hour

func serviceName(service string) string {
	switch service {
	case state.ServicePatchwork:
		return "Patchwork"
	case state.ServicePatchworkChecks:
		return "Posting checks to Patchwork"
	case state.ServiceGitHub:
		return "GitHub"
	case state.ServiceWorkflows:
		return "Workflows on the test repository"
	case state.ServiceEmail:
		return "Email"
	case state.OutageArchive(reportsCursor):
		return "The reports archive"
	case state.OutageArchive(devCursor):
		return "The dev list archive"
	}
	return service
}

// unavailable records that a service is unavailable and returns the
// period, which may have started earlier.
func (p *Pipeline) unavailable(service string, err error) *state.Period {
	a := p.Store.Availability
	if per := a.Failing[service]; per != nil {
		return per
	}
	per := a.Unavailable(service, oneLine(err.Error()), p.now())
	p.infof("%s is unavailable since %s: %s", serviceName(service), per.Since, per.Reason)
	return per
}

// available records that a service is available, which ends its period
// of unavailability, if any.
func (p *Pipeline) available(service string) {
	if per := p.Store.Availability.Available(service, p.now()); per != nil {
		p.infof("%s is available again; it was unavailable since %s.",
			serviceName(service), per.Since)
	}
}

// outageService is the service of an outage.
func outageService(kind string, o *state.Outage) string {
	switch kind {
	case state.OutageRuns, state.OutageDispatch:
		return state.ServiceWorkflows
	case state.OutagePatchwork:
		if o != nil && o.Checks {
			return state.ServicePatchworkChecks
		}
	}
	return kind
}

// outageStarted marks the period of the service of an outage as an
// outage, starting one when the service had none.
func (p *Pipeline) outageStarted(kind string, o *state.Outage) {
	a := p.Store.Availability
	service := outageService(kind, o)
	per := a.Failing[service]
	if per == nil {
		per = a.Unavailable(service, o.Reason, state.ParseTime(o.Since))
	}
	if o.Since < per.Since {
		per.Since = o.Since
	}
	per.Outage = true
}

// ghUnavailable reports whether an error says that GitHub is
// unavailable to the bot, rather than that one request is wrong.
func ghUnavailable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if ghapp.IsAuthError(err) {
		return true
	}
	var se *httpx.StatusError
	if !errors.As(err, &se) {
		return true
	}
	return se.Code == http.StatusUnauthorized || se.Code == http.StatusTooManyRequests ||
		se.Code >= 500 || se.RateLimited
}

// gh is the GitHub client as the stages use it, which records whether
// GitHub is available.
func (p *Pipeline) gh() ghWatch { return ghWatch{p} }

type ghWatch struct{ p *Pipeline }

var _ GitHub = ghWatch{}

func (g ghWatch) note(err error) error {
	switch {
	case err == nil:
		g.p.available(state.ServiceGitHub)
	case ghUnavailable(err):
		g.p.unavailable(state.ServiceGitHub, err)
	}
	return err
}

func (g ghWatch) RefSHA(ctx context.Context, name string) (string, error) {
	sha, err := g.p.GH.RefSHA(ctx, name)
	return sha, g.note(err)
}

func (g ghWatch) SetBranch(ctx context.Context, branch, sha string) error {
	return g.note(g.p.GH.SetBranch(ctx, branch, sha))
}

func (g ghWatch) DeleteRef(ctx context.Context, name string) error {
	return g.note(g.p.GH.DeleteRef(ctx, name))
}

func (g ghWatch) MatchingRefs(ctx context.Context, prefix string) (map[string]string, error) {
	refs, err := g.p.GH.MatchingRefs(ctx, prefix)
	return refs, g.note(err)
}

func (g ghWatch) RunsForSHA(ctx context.Context, sha string) ([]gh.WorkflowRun, error) {
	runs, err := g.p.GH.RunsForSHA(ctx, sha)
	return runs, g.note(err)
}

func (g ghWatch) DispatchRuns(ctx context.Context, branch string,
	since time.Time) ([]gh.WorkflowRun, error) {
	runs, err := g.p.GH.DispatchRuns(ctx, branch, since)
	return runs, g.note(err)
}

func (g ghWatch) ActiveRuns(ctx context.Context) ([]gh.WorkflowRun, error) {
	runs, err := g.p.GH.ActiveRuns(ctx)
	return runs, g.note(err)
}

func (g ghWatch) RunsSince(ctx context.Context, since time.Time) ([]gh.WorkflowRun, error) {
	runs, err := g.p.GH.RunsSince(ctx, since)
	return runs, g.note(err)
}

func (g ghWatch) Run(ctx context.Context, id int64) (gh.WorkflowRun, error) {
	r, err := g.p.GH.Run(ctx, id)
	return r, g.note(err)
}

func (g ghWatch) Jobs(ctx context.Context, runID int64) ([]gh.Job, error) {
	jobs, err := g.p.GH.Jobs(ctx, runID)
	return jobs, g.note(err)
}

func (g ghWatch) JobLog(ctx context.Context, jobID int64) (string, error) {
	log, err := g.p.GH.JobLog(ctx, jobID)
	return log, g.note(err)
}

func (g ghWatch) CheckRuns(ctx context.Context, suiteID int64) ([]gh.CheckRun, error) {
	runs, err := g.p.GH.CheckRuns(ctx, suiteID)
	return runs, g.note(err)
}

func (g ghWatch) Annotations(ctx context.Context, checkRunID int64) ([]gh.Annotation, error) {
	list, err := g.p.GH.Annotations(ctx, checkRunID)
	return list, g.note(err)
}

func (g ghWatch) Rerun(ctx context.Context, runID int64, failedOnly bool) error {
	return g.note(g.p.GH.Rerun(ctx, runID, failedOnly))
}

func (g ghWatch) Dispatch(ctx context.Context, file, ref string,
	inputs map[string]string) error {
	return g.note(g.p.GH.Dispatch(ctx, file, ref, inputs))
}

func (g ghWatch) WorkflowFiles(ctx context.Context, ref string) ([]string, error) {
	files, err := g.p.GH.WorkflowFiles(ctx, ref)
	return files, g.note(err)
}

func (g ghWatch) Workflows(ctx context.Context) ([]gh.Workflow, error) {
	list, err := g.p.GH.Workflows(ctx)
	return list, g.note(err)
}

func (g ghWatch) File(ctx context.Context, path, ref string) ([]byte, error) {
	data, err := g.p.GH.File(ctx, path, ref)
	return data, g.note(err)
}
