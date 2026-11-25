// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"fmt"
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
	last := string(e.mail.sent[len(e.mail.sent)-2].Data)
	if !strings.Contains(last, "|warning| pw6001") ||
		!strings.Contains(last, "WARNING: Line is 80 characters long") {
		t.Errorf("report:\n%s", last)
	}
	reply := string(e.mail.sent[len(e.mail.sent)-1].Data)
	if !strings.Contains(reply, `"github-robot: Checkpatch" passed with warnings`) ||
		!strings.Contains(reply, "WARNING: Line is 80 characters long") {
		t.Errorf("reply:\n%s", reply)
	}
	d := decision(t, e.series(60).Patches[0].Decisions, "workflow-warnings", "")
	if !strings.HasPrefix(d.Detail, "Checkpatch run 1001, attempt 1 passed with 1 warning:\n"+
		"WARNING: Line is 80 characters long") ||
		d.WorkflowURL != "https://github.com/ipsum/lorem/actions/runs/1001/attempts/1" {
		t.Errorf("decision = %+v", d)
	}
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

func TestWarningAnnotationIsCapped(t *testing.T) {
	var lines []string
	for i := range 150 {
		lines = append(lines, fmt.Sprintf("WARNING: line %d", i))
	}
	e, _ := annotatedRun(t, gh.Annotation{Level: "warning", Title: "leadtest",
		Message: strings.Join(lines, "\n")})
	e.step()
	r := checkpatchRun(e)
	if !strings.HasSuffix(r.Details, "WARNING: line 99\n... 50 more lines, see the workflow run.") {
		t.Fatalf("details:\n%s", r.Details)
	}

	long := strings.Repeat("WARNING: "+strings.Repeat("x", 191)+"\n", 60)
	e, _ = annotatedRun(t, gh.Annotation{Level: "warning", Title: "leadtest",
		Message: long})
	e.step()
	r = checkpatchRun(e)
	if !strings.HasSuffix(r.Details, "\n... longer than 10000 characters, see the workflow run.") ||
		len(r.Details) > len("Warnings:\n")+10000+100 {
		t.Fatalf("details: %d bytes, ends %q", len(r.Details),
			r.Details[max(0, len(r.Details)-80):])
	}
}

// failedCheckpatch finishes a failed Checkpatch run of patch 6001 whose
// job has the given log and annotations.
func failedCheckpatch(t *testing.T, log string, notes ...gh.Annotation) *env {
	e := newEnv(t, "")
	e.pw.addSeries(60, 1, "lorem", e.now)
	e.step()
	id := e.gh.startRuns("series_60", "sha-6001", "Checkpatch")[0]
	e.gh.jobs[id] = []gh.Job{{ID: 61, Name: "checkpatch", Conclusion: "failure",
		Steps: []gh.Step{{Name: "Run checkpatch", Conclusion: "failure"}}}}
	e.gh.logs[61] = log
	e.gh.annotations[61] = notes
	e.gh.finishRun(id, "failure")
	return e
}

const checkpatchLog = "2026-03-10T12:00:00.1Z \x1b[36;1mexit \"$status\"\x1b[0m\n" +
	"2026-03-10T12:00:00.2Z ERROR: Author should not be mailing list.\n" +
	"2026-03-10T12:00:00.3Z ##[error]Process completed with exit code 1.\n"

func TestErrorAnnotationReplacesLog(t *testing.T) {
	e := failedCheckpatch(t, checkpatchLog,
		gh.Annotation{Level: "failure", Title: "leadtest",
			Message: "== Checking \"patch.mbox\" ==\nERROR: Author should not be mailing " +
				"list.\nLines checked: 121, Warnings: 0, Errors: 1\n"},
		gh.Annotation{Level: "failure", Message: "Process completed with exit code 1."},
		gh.Annotation{Level: "warning", Title: "leadtest", Message: "WARNING: ipsum"})
	e.step()
	report := string(e.mail.sent[len(e.mail.sent)-2].Data)
	reply := string(e.mail.sent[len(e.mail.sent)-1].Data)
	for name, mail := range map[string]string{"report": report, "reply": reply} {
		if !strings.Contains(mail, "|fail| pw6001") && name == "report" ||
			!strings.Contains(mail, "checkpatch: failed at step \"Run checkpatch\"") ||
			!strings.Contains(mail, "Errors of \"checkpatch\":\r\n== Checking \"patch.mbox\""+
				" ==\r\nERROR: Author should not be mailing list.\r\nLines checked: 121, "+
				"Warnings: 0, Errors: 1\r\n") ||
			strings.Contains(mail, "log lines") || strings.Contains(mail, "exit code") ||
			strings.Contains(mail, "\x1b") || strings.Contains(mail, "WARNING: ipsum") {
			t.Errorf("%s:\n%s", name, mail)
		}
	}
}

func TestErrorAnnotationsOfAJobAreJoined(t *testing.T) {
	e := failedCheckpatch(t, checkpatchLog,
		gh.Annotation{Level: "failure", Title: "leadtest", Message: "ERROR: lorem\n"},
		gh.Annotation{Level: "failure", Title: "leadtest", Message: "ERROR: ipsum"})
	e.step()
	report := string(e.mail.sent[len(e.mail.sent)-2].Data)
	if !strings.Contains(report, "Errors of \"checkpatch\":\r\nERROR: lorem\r\n"+
		"ERROR: ipsum\r\n") {
		t.Errorf("report:\n%s", report)
	}
}

func TestErrorAnnotationIsCapped(t *testing.T) {
	var lines []string
	for i := range 150 {
		lines = append(lines, fmt.Sprintf("ERROR: line %d", i))
	}
	e := failedCheckpatch(t, checkpatchLog, gh.Annotation{Level: "failure",
		Title: "leadtest", Message: strings.Join(lines, "\n")})
	e.step()
	report := string(e.mail.sent[len(e.mail.sent)-2].Data)
	if !strings.Contains(report, "ERROR: line 99\r\n... 50 more lines, see the workflow run.") ||
		strings.Contains(report, "ERROR: line 100") {
		t.Errorf("report:\n%s", report)
	}

	long := strings.Repeat("ERROR: "+strings.Repeat("x", 193)+"\n", 60)
	e = failedCheckpatch(t, checkpatchLog, gh.Annotation{Level: "failure",
		Title: "leadtest", Message: long})
	e.step()
	// Each line has 201 characters, so 49 of them fit in 10000.
	report = string(e.mail.sent[len(e.mail.sent)-2].Data)
	if !strings.Contains(report, "x\r\n... longer than 10000 characters, see the workflow "+
		"run.") || strings.Count(report, "ERROR: x") != 49 {
		t.Errorf("report (%d errors):\n%s", strings.Count(report, "ERROR: x"), report)
	}
}

func TestErrorAnnotationFallsBackToLog(t *testing.T) {
	note := gh.Annotation{Level: "failure", Title: "leadtest", Message: "ERROR: lorem"}
	cases := map[string]func(e *env){
		"no annotation": func(e *env) { e.gh.annotations[61] = nil },
		"no permission": func(e *env) { e.gh.annotationErr = &httpx.StatusError{Code: 403} },
		"not found":     func(e *env) { e.gh.annotationErr = &httpx.StatusError{Code: 404} },
		"empty message": func(e *env) {
			e.gh.annotations[61] = []gh.Annotation{{Level: "failure", Title: "leadtest"}}
		},
		"disabled": func(e *env) { e.cfg.Workflows.WarningAnnotation = "" },
	}
	for name, setup := range cases {
		e := failedCheckpatch(t, checkpatchLog, note)
		setup(e)
		e.step()
		report := string(e.mail.sent[len(e.mail.sent)-2].Data)
		if !strings.Contains(report, "Last 25 log lines of \"checkpatch\":") ||
			strings.Contains(report, "Errors of") {
			t.Errorf("%s: report:\n%s", name, report)
		}
		if name == "disabled" && e.gh.annotationCalls != 0 {
			t.Errorf("disabled: %d annotation calls", e.gh.annotationCalls)
		}
	}
}

func TestErrorAnnotationRetry(t *testing.T) {
	e := failedCheckpatch(t, checkpatchLog,
		gh.Annotation{Level: "failure", Title: "leadtest", Message: "ERROR: lorem"})
	e.gh.fail["annotations"] = 1
	e.step()
	r := e.series(60).Patches[0].Run(state.RunPush, "Checkpatch")
	if r.Final.Email != "" || r.Failure == nil || r.Failure.Op != "logs" {
		t.Fatalf("a transient error must delay the report: %+v", r)
	}
	e.advance(10 * time.Minute)
	e.step()
	report := string(e.mail.sent[len(e.mail.sent)-2].Data)
	if r.Final.Email == "" || !strings.Contains(report, "Errors of \"checkpatch\":\r\n"+
		"ERROR: lorem\r\n") {
		t.Errorf("run = %+v, report:\n%s", r, report)
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
