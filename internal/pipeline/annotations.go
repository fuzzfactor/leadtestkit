// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"leadtestkit/internal/gh"
	"leadtestkit/internal/httpx"
	"leadtestkit/internal/state"
)

// successResult turns a successful run into a warning when one of its
// jobs has warning annotations titled workflows.warning_annotation; a
// workflow uses them to report problems that should not fail it, like
// checkpatch warnings.  It returns false while the annotations cannot
// be read and are retried later.
func (p *Pipeline) successResult(ctx context.Context, pt *state.Patch, r *state.Run,
	wr gh.WorkflowRun) (string, bool) {
	title := p.Cfg.Workflows.WarningAnnotation
	if title == "" || wr.CheckSuiteID == 0 {
		return state.ResultSuccess, true
	}
	if r.Failure != nil && r.Failure.Op == "annotations" && !r.Failure.Due(p.now()) {
		p.infof("%s: waiting until %s to read the annotations of %s run %d.",
			patchName(pt), r.Failure.Next, r.Workflow, wr.ID)
		return "", false
	}
	msgs, err := p.warningAnnotations(ctx, wr.CheckSuiteID, title)
	ours := r.Failure == nil || r.Failure.Op == "annotations"
	if err != nil {
		code := httpx.StatusCode(err)
		switch {
		case code == http.StatusForbidden || code == http.StatusNotFound:
			p.warnf("%s: cannot read the annotations of %s run %d; reporting it "+
				"as successful (the GitHub App needs Checks: read-only): %v",
				patchName(pt), r.Workflow, wr.ID, err)
		case !ours:
			// The failure slot belongs to a pending report; retry
			// without a backoff of our own.
			p.warnf("%s: cannot read the annotations of %s run %d: %v",
				patchName(pt), r.Workflow, wr.ID, err)
			return "", false
		case !p.fail(&r.Failure, "annotations", err):
			p.noteRun(pt, wr.HTMLURL, wr.RunAttempt, "annotations-failed",
				"%s run %d, attempt %d: reading its annotations failed "+
					"%d times: %s.", r.Workflow, wr.ID, wr.RunAttempt,
				r.Failure.Attempts, r.Failure.LastError)
			return "", false
		default:
			p.warnf("%s: reporting %s run %d as successful without its annotations.",
				patchName(pt), r.Workflow, wr.ID)
		}
		if ours {
			r.Failure = nil
		}
		return state.ResultSuccess, true
	}
	if ours {
		r.Failure = nil
	}
	if len(msgs) == 0 {
		return state.ResultSuccess, true
	}
	warnings := headLines(strings.Join(msgs, "\n"), p.Cfg.Report.LogLines)
	r.Details = "Warnings:\n" + warnings
	p.noteRun(pt, wr.HTMLURL, wr.RunAttempt, "workflow-warnings",
		"%s run %d, attempt %d passed with %s:\n%s", r.Workflow, wr.ID, wr.RunAttempt,
		plural(len(msgs), "warning"), warnings)
	p.infof("%s: %s run %d passed with %d %q warning annotations; reporting a warning.",
		patchName(pt), r.Workflow, wr.ID, len(msgs), title)
	return state.ResultWarning, true
}

func (p *Pipeline) warningAnnotations(ctx context.Context, suiteID int64,
	title string) ([]string, error) {
	runs, err := p.gh().CheckRuns(ctx, suiteID)
	if err != nil {
		return nil, err
	}
	var msgs []string
	for _, cr := range runs {
		if cr.Output.AnnotationsCount == 0 {
			continue
		}
		notes, err := p.gh().Annotations(ctx, cr.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range notes {
			if a.Level == "warning" && a.Title == title {
				msgs = append(msgs, strings.TrimRight(a.Message, "\n"))
			}
		}
	}
	return msgs, nil
}

func headLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[:n], "\n") +
		fmt.Sprintf("\n... %d more lines, see the workflow run.", len(lines)-n)
}
