// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"strings"
	"testing"
	"time"

	"leadtestkit/internal/gh"
	"leadtestkit/internal/httpx"
	"leadtestkit/internal/state"
)

// annotatedRun finishes a successful Checkpatch run of patch 6001 whose
// jobs carry the given annotations.
func annotatedRun(t *testing.T, notes ...gh.Annotation) (*env, int64) {
	e := newEnv(t, "")
	e.pw.addSeries(60, 1, "lorem", e.now)
	e.step()
	id := e.gh.startRuns("series_60", "sha-6001", "Checkpatch")[0]
	e.gh.jobs[id] = []gh.Job{{ID: 61, Name: "checkpatch", Conclusion: "success"},
		{ID: 62, Name: "other", Conclusion: "success"}}
	e.gh.annotations[61] = notes
	e.gh.finishRun(id, "success")
	return e, id
}

func checkpatchRun(e *env) *state.Run {
	return e.series(60).Patches[0].Run(state.RunPush, "Checkpatch")
}

func TestWarningAnnotationMakesWarning(t *testing.T) {
	e, _ := annotatedRun(t,
		gh.Annotation{Level: "warning", Title: "leadtest",
			Message: "WARNING: Line is 80 characters long (recommended limit is 79)\n" +
				"#20 FILE: lib/lorem.c:12:\n"},
		gh.Annotation{Level: "failure", Message: "Process completed with exit code 1."},
		gh.Annotation{Level: "warning", Title: "Node.js 20 actions are deprecated"})
	e.step()
	r := checkpatchRun(e)
	if r.Result != state.ResultWarning ||
		!strings.Contains(r.Details, "WARNING: Line is 80 characters long") ||
		strings.Contains(r.Details, "Node.js") || strings.Contains(r.Details, "exit code") {
		t.Fatalf("run = %+v", r)
	}
	if e.gh.annotationCalls != 1 {
		t.Errorf("annotations of jobs without any were read: %d calls",
			e.gh.annotationCalls)
	}
	last := string(e.mail.sent[len(e.mail.sent)-1].Data)
	if !strings.Contains(last, "|warning| pw6001") ||
		!strings.Contains(last, "WARNING: Line is 80 characters long") {
		t.Errorf("report:\n%s", last)
	}
	decision(t, e.series(60).Patches[0].Decisions, "workflow-warnings", "")
}

func TestOtherAnnotationsKeepSuccess(t *testing.T) {
	e, _ := annotatedRun(t,
		gh.Annotation{Level: "warning", Title: "Node.js 20 actions are deprecated"},
		gh.Annotation{Level: "notice", Title: "leadtest", Message: "lorem"})
	e.step()
	if r := checkpatchRun(e); r.Result != state.ResultSuccess || r.Details != "" {
		t.Fatalf("run = %+v", r)
	}
}

func TestWarningAnnotationsDisabled(t *testing.T) {
	e, _ := annotatedRun(t, gh.Annotation{Level: "warning", Title: "leadtest"})
	e.cfg.Workflows.WarningAnnotation = ""
	e.step()
	if r := checkpatchRun(e); r.Result != state.ResultSuccess || e.gh.annotationCalls != 0 {
		t.Fatalf("run = %+v, %d annotation calls", r, e.gh.annotationCalls)
	}
}

func TestAnnotationErrors(t *testing.T) {
	notes := gh.Annotation{Level: "warning", Title: "leadtest", Message: "WARNING: lorem"}
	e, _ := annotatedRun(t, notes)
	e.gh.fail["annotations"] = 1
	e.step()
	r := checkpatchRun(e)
	if r.Result != "" || r.Failure == nil || r.Failure.Op != "annotations" {
		t.Fatalf("a transient error must delay the result: %+v", r)
	}
	e.step()
	if r.Result != "" {
		t.Fatal("annotations were read again before the retry was due")
	}
	e.advance(10 * time.Minute)
	e.step()
	if r.Result != state.ResultWarning || r.Failure != nil {
		t.Fatalf("run after the retry = %+v", r)
	}

	e, _ = annotatedRun(t, notes)
	e.gh.annotationErr = &httpx.StatusError{Code: 403}
	e.step()
	if r := checkpatchRun(e); r.Result != state.ResultSuccess || r.Failure != nil {
		t.Fatalf("without Checks: read the run must be reported: %+v", r)
	}
}
