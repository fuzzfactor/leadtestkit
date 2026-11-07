// Copyright 2026 Leadlight Authors
// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from leadlight tui/apply.go and tui/mbox.go (commit 1f9d0e7), modified.

package pw

import (
	"fmt"
	"mime"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
)

var headerFoldRe = regexp.MustCompile(`\r?\n[ \t]+`)

// UpgradeHTTP converts http:// URLs to https://.  Patchwork reports
// http:// links even when it is served over https://.
func UpgradeHTTP(u string) string {
	if strings.HasPrefix(u, "http://") {
		return "https://" + u[len("http://"):]
	}
	return u
}

func compactHeader(s string) string {
	return strings.TrimSpace(headerFoldRe.ReplaceAllString(s, " "))
}

func decodeHeader(s string) string {
	dec := new(mime.WordDecoder)
	result, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return result
}

// Header returns a decoded header value from the Patchwork headers
// map, where a value is either a string or a list of strings.
func Header(headers map[string]any, key string) string {
	var v any
	for k, val := range headers {
		if strings.EqualFold(k, key) {
			v = val
			break
		}
	}
	var s string
	switch val := v.(type) {
	case string:
		s = val
	case []any:
		if len(val) > 0 {
			s, _ = val[0].(string)
		}
	}
	return decodeHeader(compactHeader(s))
}

// dmarcFromRe matches From headers rewritten by mailing lists for DMARC,
// e.g. "Lorem Ipsum via dev <dev@lists.example.org>".
var dmarcFromRe = regexp.MustCompile(`(?i)\svia\s+\S+`)

// Author returns the patch author.  Mailing lists rewrite From for
// DMARC and keep the original sender in Reply-To.
func Author(p *PatchDetail) string {
	from := Header(p.Headers, "From")
	if replyTo := Header(p.Headers, "Reply-To"); replyTo != "" &&
		(from == "" || dmarcFromRe.MatchString(from)) {
		return replyTo
	}
	if from != "" {
		return from
	}
	return formatAddress(p.Submitter.Name, p.Submitter.Email)
}

func formatAddress(name, email string) string {
	if email == "" {
		return name
	}
	return (&mail.Address{Name: name, Address: email}).String()
}

func encodeAddress(s string) string {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return mime.QEncoding.Encode("utf-8", s)
	}
	return a.String()
}

var patchNumRe = regexp.MustCompile(`\b(\d+)/\d+\]`)

// PatchNumber extracts N from "[PATCH v2 N/M] subject"; 0 if absent.
func PatchNumber(name string) int {
	m := patchNumRe.FindStringSubmatch(name)
	if len(m) > 1 {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// BuildMbox constructs a single email for `git am` from patch detail
// data, since the web mbox endpoint is usually behind bot protection.
// There is no mbox "From " separator, so body lines starting with
// "From " cannot split the message.
func BuildMbox(p *PatchDetail) ([]byte, error) {
	if strings.TrimSpace(p.Diff) == "" {
		return nil, fmt.Errorf("patch %d has no diff", p.ID)
	}
	subject := Header(p.Headers, "Subject")
	if subject == "" {
		subject = compactHeader(p.Name)
	}
	date := Header(p.Headers, "Date")
	if date == "" {
		if t := ParseDate(p.Date); !t.IsZero() {
			date = t.Format("Mon, 2 Jan 2006 15:04:05 -0000")
		}
	}
	msgID := strings.TrimSpace(p.MsgID)
	if msgID == "" {
		msgID = fmt.Sprintf("<patch-%d@patchwork>", p.ID)
	}

	var b strings.Builder
	b.WriteString("From: " + encodeAddress(Author(p)) + "\n")
	if date != "" {
		b.WriteString("Date: " + date + "\n")
	}
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", subject) + "\n")
	b.WriteString("Message-Id: " + msgID + "\n")
	b.WriteString("MIME-Version: 1.0\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\n")
	b.WriteString("\n")
	b.WriteString(p.Content)
	if !strings.HasSuffix(p.Content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("---\n")
	b.WriteString(p.Diff)
	if !strings.HasSuffix(p.Diff, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}
