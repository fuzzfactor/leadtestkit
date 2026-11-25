package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"leadtestkit/internal/gh"
)

func testFailures(log string, n int) string {
	lines, _, _ := testDetails(failedTests(log, n), nil)
	return strings.Join(lines, "\n")
}

func jobLog(lines ...string) string {
	for i, l := range lines {
		lines[i] = "2026-11-23T20:29:40.8229420Z " + l
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

const pinning = "check ovn-northd and ovn-controller version pinning -- " +
	"parallelization=yes -- ovn_monitor_all=yes"

func testBlock(n int) []string {
	lines := []string{
		"#                             -*- compilation -*-",
		fmt.Sprintf("%d. ovn.at:34206: testing %s ...", n, pinning),
	}
	for i := 1; i <= 37; i++ {
		lines = append(lines, fmt.Sprintf("test %d line %d", n, i))
	}
	return append(lines, "./ovn.at:34206: exit code was 2, expected 0",
		fmt.Sprintf("%d. ovn.at:34206: %d. %s (ovn.at:34206): FAILED (ovn.at:34206)",
			n, n, pinning))
}

func failedTestsLog() []string {
	lines := []string{
		"463: check ovn-northd -- parallelization=yes ok",
		"464: " + pinning + " FAILED (ovn.at:34206)",
		"465: " + pinning + " FAILED (ovn.at:34206)",
		"466: another test skipped (ovn.at:1)",
		"## ------------- ##",
		"## Test results. ##",
		"## ------------- ##",
		"ERROR: 285 tests were run,",
		"2 failed unexpectedly.",
		"464: " + pinning + " FAILED (ovn.at:34206)",
		"## ---------------------- ##",
		"## Detailed failed tests. ##",
		"## ---------------------- ##",
	}
	lines = append(lines, testBlock(464)...)
	lines = append(lines, testBlock(465)...)
	return append(lines, "## ../config.log ##", "config line")
}

func blockTail(n int) string {
	b := testBlock(n)
	return strings.Join(b[len(b)-25:], "\n")
}

func TestTestFailures(t *testing.T) {
	got := testFailures(jobLog(failedTestsLog()...), 25)
	want := "464: " + pinning + " FAILED (ovn.at:34206)\n" +
		"465: " + pinning + " FAILED (ovn.at:34206)\n" +
		"\nLog of test 464: " + pinning + "\n" + blockTail(464) + "\n" +
		"\nLog of test 465: " + pinning + "\n" + blockTail(465)
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTestFailuresWithoutDetails(t *testing.T) {
	log := jobLog(failedTestsLog()[:10]...)
	got := testFailures(log, 25)
	want := "464: " + pinning + " FAILED (ovn.at:34206)\n" +
		"465: " + pinning + " FAILED (ovn.at:34206)"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestTestFailuresPaddedNumbers(t *testing.T) {
	log := jobLog("  5: lorem test                         FAILED (lorem.at:5)",
		" 42: ipsum test                         FAILED (ipsum.at:42)",
		" 43: dolor test                         ok")
	got := testFailures(log, 25)
	want := "  5: lorem test                         FAILED (lorem.at:5)\n" +
		" 42: ipsum test                         FAILED (ipsum.at:42)"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	log = jobLog(append(failedTestsLog()[:13],
		"#                             -*- compilation -*-",
		"5. lorem.at:5: testing lorem test ...",
		"5. lorem.at:5: 5. lorem test (lorem.at:5): FAILED (lorem.at:5)")...)
	log = jobLog("  5: lorem test                         FAILED (lorem.at:5)") + log
	if got := testFailures(log, 25); !strings.Contains(got,
		"\nLog of test 5: lorem test\n#  ") {
		t.Errorf("no log for test 5 or name not trimmed:\n%s", got)
	}
}

func TestTestFailuresRecheck(t *testing.T) {
	log := jobLog("464: "+pinning+" FAILED (ovn.at:34206)",
		"465: "+pinning+" FAILED (ovn.at:34206)",
		"ERROR: 285 tests were run,",
		"464: "+pinning+" ok",
		"465: "+pinning+" FAILED (ovn.at:34206)")
	want := "465: " + pinning + " FAILED (ovn.at:34206)"
	if got := testFailures(log, 25); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	log = jobLog("464: "+pinning+" FAILED (ovn.at:34206)", "464: "+pinning+" ok")
	if got := testFailures(log, 25); got != "" {
		t.Errorf("all passed on recheck, got:\n%s", got)
	}
}

func TestTestFailuresNone(t *testing.T) {
	log := jobLog("1: lorem ok", "./ovn.at:1: exit code was 2, expected 0",
		"make: *** [Makefile:1: check] Error 1")
	if got := testFailures(log, 25); got != "" {
		t.Errorf("got %q", got)
	}
}

func numbered(from, to int, format string) []string {
	var lines []string
	for i := from; i <= to; i++ {
		lines = append(lines, fmt.Sprintf(format, i))
	}
	return lines
}

func TestLogTailBeforeError(t *testing.T) {
	lines := numbered(1, 40, "line %d")
	lines = append(lines, "##[error]Process completed with exit code 1.")
	lines = append(lines, numbered(1, 30, "cleanup %d")...)
	got := logTailBeforeError(jobLog(lines...), 25)
	if want := strings.Join(numbered(16, 40, "line %d"), "\n"); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	log := jobLog(numbered(1, 40, "line %d")...)
	if got, want := logTailBeforeError(log, 25), logTail(log, 25); got != want {
		t.Errorf("without error line got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCutReport(t *testing.T) {
	lines := numbered(1, 150, "line %d")
	text, whole, cutChars := cutReport(lines, 100)
	if text != strings.Join(lines[:100], "\n") || whole != 100 || cutChars {
		t.Errorf("lines: got %d whole, cutChars %v, tail %q", whole, cutChars,
			text[len(text)-20:])
	}

	long := strings.Repeat("x", 199)
	lines = strings.Split(strings.Repeat(long+"\n", 98)+long, "\n")
	text, whole, cutChars = cutReport(lines, 100)
	if !cutChars || whole != 50 || text != strings.Join(lines[:50], "\n") {
		t.Errorf("chars: got %d bytes, %d whole, cutChars %v", len(text), whole, cutChars)
	}

	text, whole, cutChars = cutReport([]string{strings.Repeat("é", 6000)}, 100)
	if !cutChars || whole != 0 || len(text) != 10000 || !utf8.ValidString(text) {
		t.Errorf("one line: got %d bytes, %d whole, cutChars %v", len(text), whole, cutChars)
	}

	if text, whole, cutChars := cutReport([]string{"short", "text"}, 100); text !=
		"short\ntext" || whole != 2 || cutChars {
		t.Errorf("short: got %q, %d, %v", text, whole, cutChars)
	}
}

func TestBuildFailureReport(t *testing.T) {
	e := newEnv(t, "")
	e.gh.jobs[77] = []gh.Job{
		{ID: 55, Name: "lorem-job", Conclusion: "failure",
			Steps: []gh.Step{{Name: "build", Conclusion: "failure"}}},
		{ID: 56, Name: "ipsum-job", Conclusion: "failure",
			Steps: []gh.Step{{Name: "build", Conclusion: "failure"}}},
	}
	e.gh.logs[55] = jobLog(append(failedTestsLog(),
		"##[error]Process completed with exit code 1.", "Post job cleanup.")...)
	e.gh.logs[56] = jobLog(append(numbered(1, 30, "compile %d"),
		"##[error]Process completed with exit code 2.", "Post job cleanup.")...)
	got, err := e.p.failureLogs(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"Failed tests in \"lorem-job\":\n464: ",
		"\nLog of test 465: " + pinning + "\n",
		"Last 25 log lines of \"ipsum-job\":\ncompile 6\n",
		"compile 30\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if strings.Contains(got, "Post job cleanup.") || strings.Contains(got, "##[error]") {
		t.Errorf("cleanup in report:\n%s", got)
	}
}

func testsLog(nums ...int) []string {
	var lines []string
	for _, n := range nums {
		lines = append(lines, fmt.Sprintf("%d: %s FAILED (ovn.at:34206)", n, pinning))
	}
	lines = append(lines, "## Detailed failed tests. ##")
	for _, n := range nums {
		lines = append(lines, testBlock(n)...)
	}
	return lines
}

func failedJobs(e *env, logs ...[]string) {
	e.gh.jobs[77] = nil
	for i, l := range logs {
		id := int64(100 + i)
		e.gh.jobs[77] = append(e.gh.jobs[77], gh.Job{ID: id,
			Name: fmt.Sprintf("job %d", i+1), Conclusion: "failure",
			Steps: []gh.Step{{Name: "build", Conclusion: "failure"}}})
		e.gh.logs[id] = jobLog(l...)
	}
}

func TestTestLogsShownAgainIfAllShown(t *testing.T) {
	e := newEnv(t, "")
	failedJobs(e, failedTestsLog(), failedTestsLog(), failedTestsLog())
	got, err := e.p.failureLogs(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"464", "465"} {
		if c := strings.Count(got, "\nLog of test "+n+": "); c != 3 {
			t.Errorf("log of test %s shown %d times:\n%s", n, c, got)
		}
	}
	for _, job := range []string{"job 1", "job 2", "job 3"} {
		// 2 logs of 27 lines.
		s := "465: " + pinning + " FAILED (ovn.at:34206)\n\n" +
			"First 54 lines of failures in \"" + job + "\":\n\nLog of test 464: "
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if strings.Contains(got, "Skipping") {
		t.Errorf("skipped logs:\n%s", got)
	}
}

func TestCutTestLogShownAgain(t *testing.T) {
	e := newEnv(t, "")
	// 4 list lines and 4 logs of 27 lines: the log of 467 is cut.
	failedJobs(e, testsLog(464, 465, 466, 467), testsLog(464, 465, 466, 467))
	got, err := e.p.failureLogs(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	_, job2, _ := strings.Cut(got, "Failed tests in \"job 2\":")
	for _, s := range []string{
		"467: " + pinning + " FAILED (ovn.at:34206)\n\n" +
			"Skipping the logs of already shown tests 464, 465, 466 (see workflow run).\n" +
			"Next 27 lines of \"job 2\":\n\nLog of test 467: ",
		"\nLog of test 467: " + pinning + "\n",
		"467. ovn.at:34206: 467. " + pinning,
	} {
		if !strings.Contains(job2, s) {
			t.Errorf("missing %q in job 2:\n%s", s, job2)
		}
	}
	if strings.Contains(job2, "Log of test 466") || strings.Contains(job2, "...") {
		t.Errorf("log of 466 shown again:\n%s", job2)
	}
}

func TestTotalLineLimit(t *testing.T) {
	e := newEnv(t, "")
	failedJobs(e, numbered(1, 30, "compile %d"),
		testsLog(411, 412, 413, 414), testsLog(421, 422, 423, 424),
		testsLog(431, 432, 433, 434), testsLog(441, 442, 443, 444))
	got, err := e.p.failureLogs(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	// 25 + 100 + 100 lines, then 75 of the 100 of job 4 and none of job 5.
	_, job4, _ := strings.Cut(got, "Failed tests in \"job 4\":\n")
	note := "\n... Total line limit reached, 125 more lines. " +
		"See the workflow run for all detailed failed tests.\n"
	body, rest, ok := strings.Cut(job4, note)
	if !ok || rest != "" {
		t.Fatalf("no total note at the end of job 4:\n%s", job4)
	}
	if n := len(strings.Split(body, "\n")); n != 75 {
		t.Errorf("job 4 has %d lines:\n%s", n, body)
	}
	if strings.Contains(got, "job 5\":") {
		t.Errorf("job 5 included:\n%s", got)
	}
	if c := strings.Count(got, "more lines, see the workflow run for the full report."); c != 2 {
		t.Errorf("%d per-job notes:\n%s", c, got)
	}
}
