package pipeline

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxReportLines = 100
	maxReportChars = 10000
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

// testFailures lists the tests that autotest reports as failed in a job
// log, each followed by the last n lines of its detailed log.  It returns
// "" if no test failed.
func testFailures(log string, n int) string {
	lines := logLines(log)
	type test struct {
		num, name, line string
		failed          bool
	}
	var order []string
	results := map[string]test{}
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
			results[m[1]] = test{m[1], m[2], l, strings.HasPrefix(m[3], "FAILED")}
		}
	}
	var tests []test
	for _, num := range order {
		if results[num].failed {
			tests = append(tests, results[num])
		}
	}
	if len(tests) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range tests {
		b.WriteString(t.line + "\n")
	}
	if details < 0 {
		return strings.TrimSuffix(b.String(), "\n")
	}
	for _, t := range tests {
		start, end := details, -1
		for i := details; i < len(lines); i++ {
			if compilationRe.MatchString(lines[i]) {
				start = i
			}
			if strings.HasPrefix(lines[i], t.num+". ") &&
				strings.Contains(lines[i], ": FAILED (") {
				end = i
				break
			}
		}
		if end < 0 {
			continue
		}
		block := lines[start : end+1]
		if len(block) > n {
			block = block[len(block)-n:]
		}
		fmt.Fprintf(&b, "\nLog of test %s: %s\n%s\n", t.num, t.name, strings.Join(block, "\n"))
	}
	return strings.TrimSuffix(b.String(), "\n")
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

// capReport cuts text to maxReportLines lines and then maxReportChars
// bytes.
func capReport(text string) string {
	note := ""
	if lines := strings.Split(text, "\n"); len(lines) > maxReportLines {
		text = strings.Join(lines[:maxReportLines], "\n")
		note = fmt.Sprintf("\n... %d more lines, see the workflow run for the full report.",
			len(lines)-maxReportLines)
	}
	if len(text) <= maxReportChars {
		return text + note
	}
	cut := text[:maxReportChars]
	if i := strings.LastIndexByte(cut, '\n'); i >= 0 {
		cut = cut[:i]
	} else {
		for len(cut) > 0 && !utf8.RuneStart(text[len(cut)]) {
			cut = cut[:len(cut)-1]
		}
	}
	return cut + fmt.Sprintf("\n... longer than %d characters, see the workflow run for the full report.",
		maxReportChars)
}
