// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"strings"
	"testing"
	"time"
)

var date = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func baseInput() Input {
	return Input{
		Style:       StyleOVS,
		Label:       "lorem-robot: Build and Test",
		Result:      "fail",
		Description: "lorem build: failed",
		PatchID:     2052,
		PatchName:   "[lorem-dev,v2,1/3] ipsum: Dolor sit.",
		PatchMsgID:  "<1@example.org>",
		PatchURL:    "https://pw.example.org/patch/2052/",
		BuildURL:    "https://github.com/ipsum/lorem/actions/runs/1",
		Extra:       "Build Logs:\nline",
		From:        "Robot <robot@example.org>",
		To:          "lorem-build@lists.example.org",
		Cc:          []string{"Lorem Ipsum <lorem@example.org>"},
		MessageID:   "<leadtest.1.2052.x.final.1@example.org>",
		Date:        date,
	}
}

func split(t *testing.T, e *Email) (string, string) {
	t.Helper()
	s := string(e.Data)
	i := strings.Index(s, "\r\n\r\n")
	if i < 0 {
		t.Fatalf("no header/body separator:\n%s", s)
	}
	return s[:i], strings.ReplaceAll(s[i+4:], "\r\n", "\n")
}

func TestComposeOVS(t *testing.T) {
	e, err := Compose(baseInput())
	if err != nil {
		t.Fatal(err)
	}
	head, body := split(t, e)
	for _, want := range []string{
		"From: \"Robot\" <robot@example.org>",
		"To: <lorem-build@lists.example.org>",
		"Cc: \"Lorem Ipsum\" <lorem@example.org>",
		"Subject: |fail| pw2052 [lorem-dev,v2,1/3] ipsum: Dolor sit.",
		"In-Reply-To: <1@example.org>",
		"References: <1@example.org>",
		"Message-ID: <leadtest.1.2052.x.final.1@example.org>",
		"Date: Tue, 10 Mar 2026 12:00:00 +0000",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("headers missing %q:\n%s", want, head)
		}
	}
	wantBody := "Test-Label: lorem-robot: Build and Test\n" +
		"Test-Status: fail\n" +
		"https://pw.example.org/patch/2052/\n\n" +
		"_lorem build: failed_\n" +
		"Build URL: https://github.com/ipsum/lorem/actions/runs/1\n\n" +
		"Build Logs:\nline\n"
	if body != wantBody {
		t.Errorf("body:\n%s\nwant:\n%s", body, wantBody)
	}
	if strings.Join(e.Recipients, ",") != "lorem-build@lists.example.org,lorem@example.org" {
		t.Errorf("recipients = %v", e.Recipients)
	}
}

func TestComposeDPDKFieldsWithinFirstLines(t *testing.T) {
	in := baseInput()
	in.Style = StyleDPDK
	in.Result = "success"
	e, _ := Compose(in)
	head, body := split(t, e)
	if !strings.Contains(head, "Subject: |SUCCESS| pw2052 ") {
		t.Errorf("headers:\n%s", head)
	}
	lines := strings.Split(body, "\n")
	joined := strings.Join(lines[:9], "\n")
	for _, want := range []string{"Test-Label: ", "Test-Status: SUCCESS",
		"/patch/2052/", "_lorem build"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q not within the first 9 lines:\n%s", want, joined)
		}
	}
}

func TestComposeTestMode(t *testing.T) {
	in := baseInput()
	in.Test = true
	e, _ := Compose(in)
	head, body := split(t, e)
	if !strings.Contains(head, "Subject: [leadtest-dryrun] |fail| pw2052") {
		t.Errorf("headers:\n%s", head)
	}
	// pw-ci searches for these substrings anywhere in the page.
	for _, bad := range []string{"Test-Label: ", "Test-Status: ",
		"https://pw.example.org", "http://pw"} {
		if strings.Contains(body, bad) {
			t.Errorf("test report contains %q:\n%s", bad, body)
		}
	}
	p, err := Parse("", body, nil, "pw.example.org", true)
	if err != nil || !p.Test || p.PatchID != 2052 || p.Status != "fail" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	if _, err := Parse("", body, nil, "pw.example.org", false); err == nil {
		t.Error("test report must be ignored outside test mode")
	}
}

func TestComposeLongLinesAndNonASCII(t *testing.T) {
	in := baseInput()
	in.PatchName = "dölor"
	in.Extra = strings.Repeat("x", 2000)
	e, _ := Compose(in)
	head, body := split(t, e)
	if !strings.Contains(head, "Subject: =?utf-8?q?") {
		t.Errorf("subject not encoded:\n%s", head)
	}
	for _, l := range strings.Split(body, "\n") {
		if len(l) > 998 {
			t.Errorf("line too long: %d", len(l))
		}
	}
}

func TestStatusAndNormalize(t *testing.T) {
	for _, r := range []string{"success", "fail", "warning", "pending"} {
		if NormalizeStatus(Status(StyleDPDK, r)) != r ||
			NormalizeStatus(Status(StyleOVS, r)) != r {
			t.Errorf("round trip failed for %s", r)
		}
	}
	if NormalizeStatus("FAILED") != "fail" || NormalizeStatus("lorem") != "" {
		t.Error("normalize")
	}
}

func TestContext(t *testing.T) {
	if got := Context("github-robot: Build and Test"); got != "github-robot-_Build_and_Test" {
		t.Errorf("got %q", got)
	}
}

func TestMessageID(t *testing.T) {
	id := MessageID("example.org", 1, 2, "Lorem-Robot: Build and Test", "final", 1)
	if id != "<leadtest.1.2.lorem-robot-build-and-test.final.1@example.org>" {
		t.Errorf("id = %q", id)
	}
	if !IsOwnMessageID(id, "example.org") || IsOwnMessageID("<x@example.org>", "example.org") ||
		IsOwnMessageID(id, "other.example") {
		t.Error("IsOwnMessageID")
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		body    string
		links   []string
		id      int
		msgid   string
		status  string
		desc    string
	}{
		{"numeric web link", "x",
			"Test-Label: lorem-robot\nTest-Status: SUCCESS\n" +
				"https://pw.example.org/patch/12/\n\n_lorem: ok_\n",
			nil, 12, "", "success", "lorem: ok"},
		{"api link in href", "x",
			"Test-Label: lab\nTest-Status: fail\nsee link\n",
			[]string{"http://pw.example.org/api/1.2/patches/34/"}, 34, "",
			"fail", "test: fail"},
		{"unversioned api link", "x",
			"Test-Label: lab\nTest-Status: warning\n" +
				"https://pw.example.org/api/patches/56/\n",
			nil, 56, "", "warning", "test: warning"},
		{"msgid link", "x",
			"Test-Label: lab\nTest-Status: pending\n" +
				"https://pw.example.org/project/lorem/patch/" +
				"20260301.1-lorem%40example.org/\n",
			nil, 0, "<20260301.1-lorem@example.org>", "pending", "test: pending"},
		{"other host ignored, subject fallback", "|FAILURE| pw78 [PATCH] x",
			"Test-Label: lab\nTest-Status: FAILURE\nhttps://other.example/patch/1/\n",
			nil, 78, "", "fail", "test: fail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Parse(tt.subject, tt.body, tt.links, "pw.example.org", false)
			if err != nil {
				t.Fatal(err)
			}
			if p.PatchID != tt.id || p.PatchMsgID != tt.msgid ||
				p.Status != tt.status ||
				p.Description != tt.desc {
				t.Errorf("parsed = %+v", p)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	for name, body := range map[string]string{
		"no label":    "Test-Status: fail\nhttps://pw.example.org/patch/1/",
		"no status":   "Test-Label: x\nhttps://pw.example.org/patch/1/",
		"bad status":  "Test-Label: x\nTest-Status: lorem\nhttps://pw.example.org/patch/1/",
		"no patch":    "Test-Label: x\nTest-Status: fail\n",
		"plain email": "Hello, lorem ipsum.",
	} {
		if _, err := Parse("no id", body, nil, "pw.example.org", false); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestComposeParseRoundTrip(t *testing.T) {
	for _, style := range []string{StyleOVS, StyleDPDK} {
		in := baseInput()
		in.Style = style
		e, _ := Compose(in)
		head, body := split(t, e)
		subject := head[strings.Index(head, "Subject: ")+9:]
		p, err := Parse(subject, body, nil, "pw.example.org", false)
		if err != nil || p.PatchID != 2052 || p.Status != "fail" || p.Label != in.Label ||
			p.Description != in.Description {
			t.Errorf("%s: p=%+v err=%v", style, p, err)
		}
	}
}

func TestComposeAlert(t *testing.T) {
	e, err := ComposeAlert(Alert{From: "Lorem Robot <robot@example.org>",
		To:      []string{"Ipsum <ipsum@example.org>", "dolor@example.org"},
		Subject: "LeadTestKit Lorem: workflows do not run on ipsum/lorem",
		Body:    "Since lorem.\n", Date: time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
		MessageID: "<leadtest.alert.1@example.org>"})
	if err != nil {
		t.Fatal(err)
	}
	data := string(e.Data)
	for _, want := range []string{
		"To: \"Ipsum\" <ipsum@example.org>, <dolor@example.org>\r\n",
		"Subject: LeadTestKit Lorem: workflows do not run on ipsum/lorem\r\n",
		"Message-ID: <leadtest.alert.1@example.org>\r\n",
		"\r\n\r\nSince lorem.\r\n",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("missing %q in:\n%s", want, data)
		}
	}
	if strings.Join(e.Recipients, " ") != "ipsum@example.org dolor@example.org" {
		t.Errorf("recipients = %v", e.Recipients)
	}
	if _, err := ComposeAlert(Alert{From: "robot@example.org"}); err == nil {
		t.Error("an alert without recipients was composed")
	}
}

func baseReply() Reply {
	return Reply{
		From:       "Lorem Robot <robot@example.org>",
		Name:       "Lorem Ipsum",
		To:         "\"Lorem Ipsum\" <lorem@example.org>",
		Cc:         "lorem-dev@lists.example.org",
		Subject:    "[lorem-dev] [PATCH v2 1/3] ipsum: Dolor sit.",
		PatchMsgID: "<1@example.org>",
		PatchURL:   "https://pw.example.org/patch/2052/",
		Label:      "lorem-robot: Build and Test",
		Result:     "fail",
		BuildURL:   "https://github.com/ipsum/lorem/actions/runs/1",
		Extra:      "Failed jobs:\n  lorem: failed at step \"make\"",
		Signature:  "Lorem Robot",
		Intended:   "\"Lorem Ipsum\" <lorem@example.org>, lorem-dev@lists.example.org",
		MessageID:  "<leadtest.1.2052.x.reply.1@example.org>",
		Date:       time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC),
	}
}

func TestComposeReply(t *testing.T) {
	e, err := ComposeReply(baseReply())
	if err != nil {
		t.Fatal(err)
	}
	head, body := split(t, e)
	for _, want := range []string{
		"From: \"Lorem Robot\" <robot@example.org>",
		"To: \"Lorem Ipsum\" <lorem@example.org>",
		"Cc: <lorem-dev@lists.example.org>",
		"Subject: Re: [lorem-dev] [PATCH v2 1/3] ipsum: Dolor sit.",
		"In-Reply-To: <1@example.org>",
		"References: <1@example.org>",
		"Message-ID: <leadtest.1.2052.x.reply.1@example.org>",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("headers missing %q:\n%s", want, head)
		}
	}
	wantBody := "Bleep bloop.  Greetings Lorem Ipsum, I am a robot and I have tried " +
		"your patch.\nThanks for your contribution.\n\n" +
		"Unfortunately, the test \"lorem-robot: Build and Test\" failed.  See the " +
		"details below.\n\n" +
		"Patch: https://pw.example.org/patch/2052/\n" +
		"Build URL: https://github.com/ipsum/lorem/actions/runs/1\n\n" +
		"Failed jobs:\n  lorem: failed at step \"make\"\n\n" +
		"If you think this is a mistake, please reply to this email.\n\n" +
		"Thanks,\nLorem Robot\n"
	if body != wantBody {
		t.Errorf("body:\n%s\nwant:\n%s", body, wantBody)
	}
	if strings.Join(e.Recipients, ",") != "lorem@example.org,lorem-dev@lists.example.org" {
		t.Errorf("recipients = %v", e.Recipients)
	}
}

func TestComposeReplyVariants(t *testing.T) {
	in := baseReply()
	in.Subject, in.Cc, in.Name, in.Apply = "RE: [PATCH] lorem", "", "", true
	e, err := ComposeReply(in)
	if err != nil {
		t.Fatal(err)
	}
	head, body := split(t, e)
	if !strings.Contains(head, "Subject: RE: [PATCH] lorem\r\n") ||
		strings.Contains(head, "Cc:") || len(e.Recipients) != 1 ||
		!strings.HasPrefix(body, "Bleep bloop.  Greetings, I am a robot") ||
		!strings.Contains(body, "Unfortunately, your patch does not apply.") {
		t.Errorf("reply:\n%s\n\n%s", head, body)
	}

	in = baseReply()
	in.Test, in.Result, in.To, in.Cc = true, "warning", "lorem-build@lists.example.org", ""
	e, err = ComposeReply(in)
	if err != nil {
		t.Fatal(err)
	}
	head, body = split(t, e)
	if !strings.Contains(head, "Subject: [leadtest-dryrun] Re: [lorem-dev] [PATCH v2") ||
		!strings.HasPrefix(body, "This is a LeadTestKit test reply; in live mode it goes "+
			"to \"Lorem Ipsum\" <lorem@example.org>, lorem-dev@lists.example.org.\n") ||
		!strings.Contains(body, "\"lorem-robot: Build and Test\" passed with warnings.") ||
		strings.Join(e.Recipients, ",") != "lorem-build@lists.example.org" {
		t.Errorf("test reply:\n%s\n\n%s", head, body)
	}
}
