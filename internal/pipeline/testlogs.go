package pipeline

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxReportLines      = 100
	maxReportChars      = 10000
	maxTotalReportLines = 300
)

var (
	testResultRe = regexp.MustCompile(`^\s*(\d+): (.*?)\s+` +
		`(ok|skipped \(.*\)|expected failure \(.*\)|UNEXPECTED PASS|FAILED \([^)]*\))$`)
	compilationRe = regexp.MustCompile(`^#\s+-\*- compilation -\*-$`)
)

func logLines(log string) []string {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(log, "\r\n", "\n"), "\n"), "\n")
	for i, l := range lines {
		lines[i] = logTimestampRe.ReplaceAllString(l, "")
	}
	return lines
}

type failedTest struct {
	num, name, line string
	log             []string // last lines of its detailed log, if found
}

func (t failedTest) key() string { return t.num + ": " + t.name }

// failedTests returns the tests that autotest reports as failed in a job
// log, with the last n lines of their detailed logs.
func failedTests(log string, n int) []failedTest {
	lines := logLines(log)
	type result struct {
		test   failedTest
		failed bool
	}
	var order []string
	results := map[string]result{}
	details := -1
	for i, l := range lines {
		if l == "## Detailed failed tests. ##" {
			details = i + 1
			break
		}
		// A recheck runs failed tests again; their last result counts.
		if m := testResultRe.FindStringSubmatch(l); m != nil {
			if _, ok := results[m[1]]; !ok {
				order = append(order, m[1])
			}
			results[m[1]] = result{failedTest{num: m[1], name: m[2], line: l},
				strings.HasPrefix(m[3], "FAILED")}
		}
	}
	var tests []failedTest
	for _, num := range order {
		r := results[num]
		if !r.failed {
			continue
		}
		t := r.test
		start := details
		for i := details; details >= 0 && i < len(lines); i++ {
			if compilationRe.MatchString(lines[i]) {
				start = i
			}
			if strings.HasPrefix(lines[i], t.num+". ") &&
				strings.Contains(lines[i], ": FAILED (") {
				t.log = lines[max(start, i+1-n) : i+1]
				break
			}
		}
		tests = append(tests, t)
	}
	return tests
}

// testDetails lists failed tests followed by their logs.  It skips the
// logs in shown, unless all are, and returns the tests it skips.  ends
// has, for each log, the number of lines up to its end.
func testDetails(tests []failedTest, shown map[string]bool) (lines []string,
	ends map[string]int, skipped []string) {
	fresh := false
	for _, t := range tests {
		lines = append(lines, t.line)
		fresh = fresh || t.log != nil && !shown[t.key()]
	}
	ends = map[string]int{}
	for _, t := range tests {
		if t.log == nil {
			continue
		}
		if fresh && shown[t.key()] {
			skipped = append(skipped, t.num)
			continue
		}
		lines = append(lines, "", fmt.Sprintf("Log of test %s: %s", t.num, t.name))
		lines = append(lines, t.log...)
		ends[t.key()] = len(lines)
	}
	return lines, ends, skipped
}

// introduceLogs puts lines before the logs after the list of n failed
// tests, saying which were skipped and how many lines of the limit
// follow.
func introduceLogs(lines []string, ends map[string]int, n int, skipped []string,
	job string, limit int) ([]string, map[string]int) {
	intro := []string{"", ""}
	if len(skipped) > 0 {
		what := "test"
		if len(skipped) > 1 {
			what = "tests"
		}
		intro = append(intro, "")
		intro[1] = fmt.Sprintf("Skipping the logs of already shown %s %s (see workflow run).",
			what, strings.Join(skipped, ", "))
	}
	follow := max(min(len(lines)+len(intro), limit)-n-len(intro), 0)
	if len(skipped) > 0 {
		intro[2] = fmt.Sprintf("Next %d lines of %q:", follow, job)
	} else {
		intro[1] = fmt.Sprintf("First %d lines of failures in %q:", follow, job)
	}
	lines = append(lines[:n:n], append(intro, lines[n:]...)...)
	for key := range ends {
		ends[key] += len(intro)
	}
	return lines, ends
}

// logTailBeforeError returns the last n lines of a job log before the
// error that ended its failed step, which skips the cleanup steps after
// it.
func logTailBeforeError(log string, n int) string {
	lines := logLines(log)
	for i, l := range lines {
		if strings.HasPrefix(l, "##[error]Process completed with exit code") {
			lines = lines[:i]
			break
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// cutReport joins at most maxLines lines and cuts them to maxReportChars
// bytes.  whole is the number of lines it keeps whole.
func cutReport(lines []string, maxLines int) (text string, whole int, cutChars bool) {
	lines = lines[:min(len(lines), maxLines)]
	size := 0
	for i, l := range lines {
		if i > 0 {
			size++
		}
		size += len(l)
		if size > maxReportChars {
			if i > 0 {
				return strings.Join(lines[:i], "\n"), i, true
			}
			cut := l[:maxReportChars]
			for len(cut) > 0 && !utf8.RuneStart(l[len(cut)]) {
				cut = cut[:len(cut)-1]
			}
			return cut, 0, true
		}
	}
	return strings.Join(lines, "\n"), len(lines), false
}
