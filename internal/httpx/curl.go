// Copyright 2026 Leadlight Authors
// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from leadlight api/curl.go (commit 1f9d0e7), modified.

package httpx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// curlConfig builds a curl config file that is fed through stdin, so
// that headers (including Authorization) never show up in argv.
// When skipUA is true, curl sends its own default User-Agent, which is
// accepted by most bot protection systems.
func curlConfig(req *http.Request, body []byte, skipUA bool, timeout time.Duration) string {
	var b strings.Builder
	b.WriteString("silent\ninclude\nlocation\n")
	fmt.Fprintf(&b, "max-time = %d\n", int(timeout.Seconds()))
	fmt.Fprintf(&b, "request = %s\n", curlQuote(req.Method))

	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if skipUA && strings.EqualFold(k, "User-Agent") {
			continue
		}
		for _, v := range req.Header[k] {
			fmt.Fprintf(&b, "header = %s\n", curlQuote(k+": "+v))
		}
	}
	if body != nil {
		fmt.Fprintf(&b, "data-binary = %s\n", curlQuote(string(body)))
	}
	fmt.Fprintf(&b, "url = %s\n", curlQuote(req.URL.String()))
	return b.String()
}

func curlQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

func execCurl(
	ctx context.Context, req *http.Request, body []byte, skipUA bool, timeout time.Duration,
) (*http.Response, error) {
	cmd := exec.CommandContext(ctx, "curl", "--config", "-")
	cmd.Stdin = strings.NewReader(curlConfig(req, body, skipUA, timeout))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		code := -1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return nil, fmt.Errorf("curl exit %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return parseCurlResponse(out, req)
}

// parseCurlResponse parses `curl -i -L` output.  Every response in the
// redirect chain (and 1xx interim responses) has a header block, but
// only the last one has a body, so header blocks are consumed while
// the remaining data starts with a status line.
func parseCurlResponse(data []byte, req *http.Request) (*http.Response, error) {
	var status string
	var headers []string
	rest := data
	for bytes.HasPrefix(rest, []byte("HTTP/")) {
		status, headers = "", nil
		for {
			i := bytes.IndexByte(rest, '\n')
			var line string
			if i < 0 {
				line, rest = string(rest), nil
			} else {
				line, rest = string(rest[:i]), rest[i+1:]
			}
			line = strings.TrimRight(line, "\r")
			if line == "" {
				break
			}
			if status == "" {
				status = line
			} else {
				headers = append(headers, line)
			}
			if rest == nil {
				break
			}
		}
	}
	if status == "" {
		return nil, fmt.Errorf("no valid HTTP response in curl output")
	}
	resp := buildResponse(status, headers, rest, req)
	if resp == nil {
		return nil, fmt.Errorf("failed to parse HTTP status line %q", status)
	}
	return resp, nil
}

func buildResponse(statusLine string, headerLines []string, body []byte,
	req *http.Request) *http.Response {
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return nil
	}
	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil
	}
	resp := &http.Response{
		StatusCode: statusCode,
		Status:     statusLine,
		Proto:      parts[0],
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}
	for _, line := range headerLines {
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		resp.Header.Add(strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+1:]))
	}
	return resp
}

func curlAvailable() bool {
	_, err := exec.LookPath("curl")
	return err == nil
}
