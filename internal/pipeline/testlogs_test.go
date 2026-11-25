package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"leadtestkit/internal/gh"
)

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

func TestCapReport(t *testing.T) {
	lines := numbered(1, 150, "line %d")
	got := capReport(strings.Join(lines, "\n"))
	want := strings.Join(lines[:100], "\n") +
		"\n... 50 more lines, see the workflow run for the full report."
	if got != want {
		t.Errorf("lines: got tail %q", got[len(got)-80:])
	}

	long := strings.Repeat("x", 199)
	text := strings.Repeat(long+"\n", 98) + long
	got = capReport(text)
	note := "\n... longer than 10000 characters, see the workflow run for the full report."
	if !strings.HasSuffix(got, note) {
		t.Fatalf("chars: got tail %q", got[len(got)-80:])
	}
	cut := strings.TrimSuffix(got, note)
	if len(cut) > 10000 || !strings.HasPrefix(text, cut+"\n") {
		t.Errorf("chars: cut to %d bytes, not at a newline", len(cut))
	}

	got = capReport(strings.Repeat(long+"\n", 150))
	if !strings.HasSuffix(got, note) || len(strings.TrimSuffix(got, note)) > 10000 {
		t.Errorf("lines and chars: got %d bytes, tail %q", len(got), got[len(got)-80:])
	}

	if got := capReport("short\ntext"); got != "short\ntext" {
		t.Errorf("short: got %q", got)
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
