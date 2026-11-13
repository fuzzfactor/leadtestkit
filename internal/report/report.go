// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

// Package report implements the CI report email format used on the
// OVS and DPDK reports lists, as documented in the development plan.
package report

import (
	"fmt"
	"mime"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	StyleOVS  = "ovs"
	StyleDPDK = "dpdk"

	testTag       = " (leadtest dry-run)"
	SubjectPrefix = "[leadtest-dryrun] "
	maxLineLength = 900
)

var dpdkStatus = map[string]string{
	"success": "SUCCESS",
	"fail":    "FAILURE",
	"warning": "WARNING",
	"pending": "PENDING",
}

// Status renders a normalized result (patchwork check state) in the
// report style.
func Status(style, result string) string {
	if style == StyleDPDK {
		if s, ok := dpdkStatus[result]; ok {
			return s
		}
	}
	return result
}

// NormalizeStatus maps report statuses of any style to patchwork check
// states, or "" if unknown.
func NormalizeStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "success", "pass", "passed", "ok":
		return "success"
	case "fail", "failure", "failed", "error":
		return "fail"
	case "warning", "warn", "warnings":
		return "warning"
	case "pending":
		return "pending"
	}
	return ""
}

// Context converts a Test-Label to a patchwork check context; some
// patchwork instances reject spaces and colons.
func Context(label string) string {
	return strings.NewReplacer(" ", "_", ":", "-").Replace(strings.TrimSpace(label))
}

type Input struct {
	Style string
	// Test marks the report so that other report parsers ignore it.
	Test bool

	Label       string
	Result      string
	Description string

	PatchID    int
	PatchName  string
	PatchMsgID string
	PatchURL   string

	BuildURL string
	Extra    string

	From      string
	To        string
	MessageID string
	Date      time.Time
}

type Email struct {
	From       string
	Recipients []string
	Data       []byte
}

func Compose(in Input) (*Email, error) {
	from, err := mail.ParseAddress(in.From)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	to, err := mail.ParseAddress(in.To)
	if err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	recipients := []string{to.Address}

	status := Status(in.Style, in.Result)
	tag := ""
	subjectPrefix := ""
	patchURL := in.PatchURL
	if in.Test {
		tag = testTag
		subjectPrefix = SubjectPrefix
		patchURL = strings.TrimPrefix(strings.TrimPrefix(patchURL, "https://"), "http://")
	}
	subject := fmt.Sprintf("%s|%s| pw%d %s", subjectPrefix, status, in.PatchID, in.PatchName)

	var h strings.Builder
	header := func(k, v string) { fmt.Fprintf(&h, "%s: %s\r\n", k, v) }
	header("From", from.String())
	header("To", to.String())
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", in.Date.Format(time.RFC1123Z))
	header("Message-ID", in.MessageID)
	if in.PatchMsgID != "" {
		header("In-Reply-To", in.PatchMsgID)
		header("References", in.PatchMsgID)
	}
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "8bit")

	// Report parsers on the list side only look at the first lines of
	// the body, so the fixed fields come first.
	var b strings.Builder
	fmt.Fprintf(&b, "Test-Label%s: %s\n", tag, in.Label)
	fmt.Fprintf(&b, "Test-Status%s: %s\n", tag, status)
	fmt.Fprintf(&b, "%s\n\n", patchURL)
	fmt.Fprintf(&b, "_%s_\n", in.Description)
	if in.BuildURL != "" {
		fmt.Fprintf(&b, "Build URL: %s\n", in.BuildURL)
	}
	if in.Test {
		b.WriteString("\nThis is a LeadTestKit test report, not a CI result.\n")
	}
	if in.Extra != "" {
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(in.Extra, "\n"))
		b.WriteString("\n")
	}

	data := h.String() + "\r\n" + crlf(limitLines(b.String()))
	return &Email{From: from.Address, Recipients: recipients, Data: []byte(data)}, nil
}

// Reply answers the email of a patch to tell its submitter that a test
// failed or warned.
type Reply struct {
	// Test sends the reply to the reports list instead, saying who it
	// would go to.
	Test bool

	From string
	// Name greets the submitter.
	Name string
	To   string
	Cc   string

	// Subject is the subject of the patch email.
	Subject    string
	PatchMsgID string
	PatchURL   string

	Label string
	// Apply is set when the patch did not apply.
	Apply    bool
	Result   string
	BuildURL string
	Extra    string
	// ReplyTo is where answers go.
	ReplyTo string
	// Invite asks for answers, when a list gets them.
	Invite bool
	// Signature signs the reply off.
	Signature string
	// Intended are the recipients in live mode, mentioned in test mode.
	Intended  string
	MessageID string
	Date      time.Time
}

var replyPrefixRe = regexp.MustCompile(`(?i)^re:\s`)

func ComposeReply(in Reply) (*Email, error) {
	from, err := mail.ParseAddress(in.From)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	to, err := mail.ParseAddress(in.To)
	if err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	var replyTo *mail.Address
	if in.ReplyTo != "" {
		if replyTo, err = mail.ParseAddress(in.ReplyTo); err != nil {
			return nil, fmt.Errorf("reply-to: %w", err)
		}
	}
	recipients := []string{to.Address}
	var cc *mail.Address
	if in.Cc != "" {
		if cc, err = mail.ParseAddress(in.Cc); err != nil {
			return nil, fmt.Errorf("cc: %w", err)
		}
		if contains(recipients, cc.Address) {
			cc = nil
		} else {
			recipients = append(recipients, cc.Address)
		}
	}
	subject := strings.TrimSpace(in.Subject)
	if !replyPrefixRe.MatchString(subject) {
		subject = "Re: " + subject
	}
	if in.Test {
		subject = SubjectPrefix + subject
	}

	var h strings.Builder
	header := func(k, v string) { fmt.Fprintf(&h, "%s: %s\r\n", k, v) }
	header("From", from.String())
	header("To", to.String())
	if cc != nil {
		header("Cc", cc.String())
	}
	if replyTo != nil {
		header("Reply-To", replyTo.String())
	}
	header("Subject", mime.QEncoding.Encode("utf-8", subject))
	header("Date", in.Date.Format(time.RFC1123Z))
	header("Message-ID", in.MessageID)
	if in.PatchMsgID != "" {
		header("In-Reply-To", in.PatchMsgID)
		header("References", in.PatchMsgID)
	}
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "8bit")

	greeting := "Greetings"
	if name := strings.TrimSpace(in.Name); name != "" {
		greeting += " " + name
	}
	problem := fmt.Sprintf("the test %q failed", in.Label)
	switch {
	case in.Apply:
		problem = "your patch does not apply"
	case in.Result == "warning":
		problem = fmt.Sprintf("the test %q passed with warnings", in.Label)
	}
	var b strings.Builder
	if in.Test {
		fmt.Fprintf(&b, "This is a LeadTestKit test reply; in live mode it goes to %s.\n\n",
			in.Intended)
	}
	fmt.Fprintf(&b, "Bleep bloop.  %s, I am a robot and I have tried your patch.\n", greeting)
	b.WriteString("Thanks for your contribution.\n\n")
	fmt.Fprintf(&b, "Unfortunately, %s.  See the details below.\n\n", problem)
	if in.PatchURL != "" {
		fmt.Fprintf(&b, "Patch: %s\n", in.PatchURL)
	}
	if in.BuildURL != "" {
		fmt.Fprintf(&b, "Build URL: %s\n", in.BuildURL)
	}
	if in.Extra != "" {
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(in.Extra, "\n"))
		b.WriteString("\n")
	}
	if in.Invite {
		b.WriteString("\nIf you think this is a mistake, please reply to this email.\n")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Thanks,\n%s\n", in.Signature)

	data := h.String() + "\r\n" + crlf(limitLines(b.String()))
	return &Email{From: from.Address, Recipients: recipients, Data: []byte(data)}, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

func limitLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if len(l) > maxLineLength {
			lines[i] = l[:maxLineLength] + "..."
		}
	}
	return strings.Join(lines, "\n")
}

// Alert is an email to the maintainers of the bot.
type Alert struct {
	From      string
	To        []string
	Subject   string
	Body      string
	Date      time.Time
	MessageID string
}

func ComposeAlert(a Alert) (*Email, error) {
	from, err := mail.ParseAddress(a.From)
	if err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	var to, recipients []string
	for _, t := range a.To {
		addr, err := mail.ParseAddress(t)
		if err != nil {
			return nil, fmt.Errorf("to: %w", err)
		}
		to = append(to, addr.String())
		recipients = append(recipients, addr.Address)
	}
	if len(recipients) == 0 {
		return nil, fmt.Errorf("no recipients")
	}
	var h strings.Builder
	header := func(k, v string) { fmt.Fprintf(&h, "%s: %s\r\n", k, v) }
	header("From", from.String())
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", a.Subject))
	header("Date", a.Date.Format(time.RFC1123Z))
	header("Message-ID", a.MessageID)
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "8bit")
	data := h.String() + "\r\n" + crlf(limitLines(strings.TrimRight(a.Body, "\n")+"\n"))
	return &Email{From: from.Address, Recipients: recipients, Data: []byte(data)}, nil
}

func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// MessageID builds a deterministic Message-ID, so a report that may
// have been sent before a crash can be recognized in the archive.
func MessageID(domain string, seriesID, patchID int, label, kind string, attempt int) string {
	slug := strings.Trim(nonSlugRe.ReplaceAllString(strings.ToLower(label), "-"), "-")
	return fmt.Sprintf("<leadtest.%d.%d.%s.%s.%d@%s>", seriesID, patchID, slug, kind,
		attempt, domain)
}

var nonSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

func IsOwnMessageID(id, domain string) bool {
	id = strings.Trim(strings.TrimSpace(id), "<>")
	return strings.HasPrefix(id, "leadtest.") && strings.HasSuffix(id, "@"+domain)
}

type Parsed struct {
	Label       string
	Status      string
	Description string
	PatchID     int
	// PatchMsgID is set when the report links the patch by Message-ID
	// instead of by number.
	PatchMsgID string
	Test       bool
}

var (
	labelRe   = regexp.MustCompile(`(?m)Test-Label: *([^\n]*)`)
	statusRe  = regexp.MustCompile(`(?m)Test-Status: *([^\n]*)`)
	labelTRe  = regexp.MustCompile(`(?m)Test-Label \(leadtest dry-run\): *([^\n]*)`)
	statusTRe = regexp.MustCompile(`(?m)Test-Status \(leadtest dry-run\): *([^\n]*)`)
	descRe    = regexp.MustCompile(`(?m)^_(.+)_\s*$`)
	urlRe     = regexp.MustCompile(`(?:https?://)?[A-Za-z0-9.-]+(?::\d+)?/` +
		`[^\s<>"']*patch[^\s<>"']*`)
	numericRe  = regexp.MustCompile(`/(?:api/(?:\d+\.\d+/)?patches|patch)/(\d+)/?$`)
	msgidRe    = regexp.MustCompile(`/project/[^/]+/patch/([^/]+)/?$`)
	subjPwIDRe = regexp.MustCompile(`\bpw(\d+)\b`)
)

// Parse extracts a report from an archived email.  Only patch links to
// pwHost count.  With acceptTest, reports marked as test reports are
// accepted too (used in test mode).
func Parse(subject, body string, links []string, pwHost string, acceptTest bool) (*Parsed, error) {
	p := &Parsed{}
	label, status := first(labelRe, body), first(statusRe, body)
	if label == "" && status == "" && acceptTest {
		label, status = first(labelTRe, body), first(statusTRe, body)
		p.Test = label != "" || status != ""
	}
	if label == "" {
		return nil, fmt.Errorf("no Test-Label")
	}
	if status == "" {
		return nil, fmt.Errorf("no Test-Status")
	}
	p.Label = label
	p.Status = NormalizeStatus(status)
	if p.Status == "" {
		return nil, fmt.Errorf("unknown Test-Status %q", status)
	}
	p.Description = first(descRe, body)
	if p.Description == "" {
		p.Description = "test: " + p.Status
	}

	candidates := append([]string(nil), links...)
	candidates = append(candidates, urlRe.FindAllString(body, -1)...)
	for _, c := range candidates {
		id, msgid := patchFromURL(c, pwHost)
		if id > 0 {
			p.PatchID = id
			return p, nil
		}
		if msgid != "" {
			p.PatchMsgID = msgid
			return p, nil
		}
	}
	if m := subjPwIDRe.FindStringSubmatch(subject); m != nil {
		p.PatchID, _ = strconv.Atoi(m[1])
		return p, nil
	}
	return nil, fmt.Errorf("no patch reference")
}

func first(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func patchFromURL(raw, pwHost string) (int, string) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(strings.TrimRight(raw, ".,;)"))
	if err != nil || !strings.EqualFold(u.Hostname(), pwHost) {
		return 0, ""
	}
	if m := numericRe.FindStringSubmatch(u.Path); m != nil {
		id, _ := strconv.Atoi(m[1])
		return id, ""
	}
	if m := msgidRe.FindStringSubmatch(u.EscapedPath()); m != nil {
		if id, err := url.PathUnescape(m[1]); err == nil && strings.Contains(id, "@") {
			return 0, "<" + strings.Trim(id, "<>") + ">"
		}
	}
	return 0, ""
}
