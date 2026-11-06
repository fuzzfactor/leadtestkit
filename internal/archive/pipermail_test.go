// Copyright 2026 Leadlight Authors
// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from leadlight api/mailarchive_test.go (commit 1f9d0e7), modified.

package archive

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"leadtestkit/internal/httpx"
)

func TestBuildArchiveURL(t *testing.T) {
	got := BuildArchiveURL(
		"https://mail.example.org/pipermail/dev/",
		2026, time.March)
	want := "https://mail.example.org/pipermail/dev/" +
		"2026-March/date.html"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestBuildArchiveURL_TrailingSlash(t *testing.T) {
	a := BuildArchiveURL(
		"https://mail.example.org/pipermail/dev",
		2026, time.January)
	b := BuildArchiveURL(
		"https://mail.example.org/pipermail/dev/",
		2026, time.January)
	if a != b {
		t.Errorf("trailing slash mismatch: %q vs %q", a, b)
	}
}

const testPipermailHTML = `<!DOCTYPE HTML>
<HTML><HEAD>
<title>The dev March 2026 Archive by date</title>
</HEAD><BODY>
<ul>
<LI><A HREF="430745.html">[ovs-dev] [PATCH net v3] Lorem ipsum dolor sit amet
</A><A NAME="430745">&nbsp;</A>
<I>Lorem Ipsum</I>

<LI><A HREF="430746.html">[ovs-dev] [PATCH 1/3] Consectetur adipiscing elit
</A><A NAME="430746">&nbsp;</A>
<I>Dolor Amet</I>

<LI><A HREF="430800.html">Re: [ovs-dev] [PATCH 1/3] Consectetur adipiscing elit
</A><A NAME="430800">&nbsp;</A>
<I>Sit Amet</I>

<LI><A HREF="030850.html">[ovs-dev] [syzbot ci] Re: Lorem ipsum dolor sit amet
</A><A NAME="030850">&nbsp;</A>
<I>Syzbot</I>

</ul>
</BODY></HTML>`

func TestParseArchiveMessages(t *testing.T) {
	msgs, err := parseArchiveMessages(
		[]byte(testPipermailHTML))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("len = %d, want 4", len(msgs))
	}

	if msgs[0].Number != 430745 {
		t.Errorf("[0].Number = %d", msgs[0].Number)
	}
	want := "[ovs-dev] [PATCH net v3] Lorem ipsum dolor sit amet"
	if msgs[0].Subject != want {
		t.Errorf("[0].Subject = %q", msgs[0].Subject)
	}

	if msgs[2].Number != 430800 {
		t.Errorf("[2].Number = %d", msgs[2].Number)
	}

	if msgs[3].Number != 30850 {
		t.Errorf("[3].Number = %d", msgs[3].Number)
	}
}

func TestParseArchiveMessages_Empty(t *testing.T) {
	html := `<HTML><BODY><ul></ul></BODY></HTML>`
	msgs, err := parseArchiveMessages([]byte(html))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("len = %d, want 0", len(msgs))
	}
}

func TestExtractPatchCore(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"[ovs-dev] [PATCH v2 1/3] Fix something", "Fix something"},
		{"[ovs-dev,v2,1/3] Fix something", "Fix something"},
		{"[PATCH] Fix something", "Fix something"},
		{"Fix something", "Fix something"},
		{"[a] [b] [c] Fix", "Fix"},
		{"", ""},
	}
	for _, tt := range tests {
		got := ExtractPatchCore(tt.input)
		if got != tt.want {
			t.Errorf("ExtractPatchCore(%q) = %q, want %q",
				tt.input, got, tt.want)
		}
	}
}

func TestSubjectMatch_Verbatim(t *testing.T) {
	tests := []struct {
		core    string
		subject string
		want    bool
	}{
		{
			"Fix race condition in ovsdb",
			"Re: [ovs-dev] [PATCH v2] Fix race condition in ovsdb",
			true,
		},
		{
			"Fix race condition in ovsdb",
			"[ovs-dev] Fix race condition in ovsdb",
			true,
		},
		{
			"Fix race condition",
			"Something completely different",
			false,
		},
		{"", "Re: something", false},
	}
	for _, tt := range tests {
		got := subjectMatch(tt.core, tt.subject)
		if got != tt.want {
			t.Errorf(
				"subjectMatch(%q, %q) = %v, want %v",
				tt.core, tt.subject, got, tt.want)
		}
	}
}

func TestSubjectMatch_ChineseReply(t *testing.T) {
	got := subjectMatch(
		"Fix race condition in ovsdb",
		"回复: [ovs-dev] Fix race condition in ovsdb")
	if !got {
		t.Error("should match Chinese reply prefix")
	}
}

func TestSubjectMatch_JapaneseReply(t *testing.T) {
	got := subjectMatch(
		"Fix race condition",
		"返信: [dev] Fix race condition")
	if !got {
		t.Error("should match Japanese reply prefix")
	}
}

func TestSubjectMatch_NestedReplyFwd(t *testing.T) {
	got := subjectMatch(
		"Fix race condition in ovsdb",
		"Re: Fwd: [ovs-dev] Fix race condition in ovsdb")
	if !got {
		t.Error("should match nested Re: Fwd:")
	}
}

func TestSubjectMatch_Was(t *testing.T) {
	got := subjectMatch(
		"Actual subject here",
		"Re: Fwd: Was: Actual subject here")
	if !got {
		t.Error("should match through Was:")
	}
}

func TestSubjectMatch_Truncated(t *testing.T) {
	got := subjectMatch(
		"Fix race condition in ovsdb server",
		"Re: [ovs-dev] Fix race condition in ovsdb serv")
	if !got {
		t.Error("should match truncated subject (>80%)")
	}
}

func TestSubjectMatch_TooShortTruncation(t *testing.T) {
	got := subjectMatch(
		"Fix race condition in ovsdb server",
		"Re: [ovs-dev] Fix race")
	if got {
		t.Error("should NOT match heavily truncated (<80%)")
	}
}

func TestSubjectMatch_CaseInsensitive(t *testing.T) {
	got := subjectMatch(
		"Fix Race Condition",
		"RE: [DEV] fix race condition")
	if !got {
		t.Error("should match case-insensitively")
	}
}

func TestMatchPatchSubjects(t *testing.T) {
	msgs := []ArchiveMessage{
		{Number: 100,
			Subject: "Re: [ovs-dev] [PATCH] Fix bug"},
		{Number: 101,
			Subject: "[ovs-dev] Unrelated topic"},
		{Number: 102,
			Subject: "Re: [ovs-dev] [PATCH v2] Add feature"},
	}

	patchNames := map[int]string{
		1000: "[ovs-dev,v1] Fix bug",
		1001: "[ovs-dev] [PATCH v2] Add feature",
		1002: "[ovs-dev] Something else entirely",
	}

	ids := MatchPatchSubjects(msgs, patchNames)
	got := map[int]bool{}
	for _, id := range ids {
		got[id] = true
	}

	if !got[1000] {
		t.Error("patch 1000 should match (Fix bug)")
	}
	if !got[1001] {
		t.Error("patch 1001 should match (Add feature)")
	}
	if got[1002] {
		t.Error("patch 1002 should not match")
	}
}

func TestMatchPatchSubjects_NoMatches(t *testing.T) {
	msgs := []ArchiveMessage{
		{Number: 100, Subject: "[dev] Totally different"},
	}
	patchNames := map[int]string{
		1000: "[dev] Some patch name",
	}
	ids := MatchPatchSubjects(msgs, patchNames)
	if len(ids) != 0 {
		t.Errorf("got %v, want empty", ids)
	}
}

func TestLongestCommonSubstring(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"abcdef", "xbcdey", 4},
		{"hello world", "hello world", 11},
		{"abc", "xyz", 0},
		{"", "abc", 0},
		{"fix race condition", "fix race cond", 13},
	}
	for _, tt := range tests {
		got := longestCommonSubstring(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("lcs(%q, %q) = %d, want %d",
				tt.a, tt.b, got, tt.want)
		}
	}
}

func testPipermail(t *testing.T, h http.HandlerFunc) (*Pipermail, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	hc := httpx.New(httpx.Options{
		HTTPClient: srv.Client(),
		CurlFirst:  true,
		Logger:     slog.New(slog.DiscardHandler),
		Sleep:      func(ctx context.Context, d time.Duration) error { return nil },
	})
	return NewPipermail(srv.URL+"/pipermail/dev/", hc), srv
}

func TestFetchMonth(t *testing.T) {
	p, _ := testPipermail(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pipermail/dev/2026-March/date.html" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(testPipermailHTML))
	})
	msgs, err := p.FetchMonth(context.Background(), 2026, time.March)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Errorf("len = %d, want 4", len(msgs))
	}
}

func TestFetchMonthMissing(t *testing.T) {
	p, _ := testPipermail(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	})
	msgs, err := p.FetchMonth(context.Background(), 2026, time.April)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("msgs=%v err=%v", msgs, err)
	}
}

func TestFetchMonthBotChallenge(t *testing.T) {
	p, _ := testPipermail(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Making sure you're not a bot!"))
	})
	if _, err := p.FetchMonth(context.Background(), 2026, time.March); err == nil {
		t.Error("expected error for bot challenge page")
	}
}

const testArticleHTML = `<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 4.01 Transitional//EN">
<HTML>
 <HEAD>
   <TITLE> [lorem-build] |fail| pw2052 [lorem-dev] ipsum: Dolor sit.
   </TITLE>
   <LINK REL="Index" HREF="index.html" >
   <LINK REL="made" HREF="mailto:b%40l.example.org?In-Reply-To=%3C1.abc%40r.example.org%3E">
 </HEAD>
 <BODY BGCOLOR="#ffffff">
   <H1>[lorem-build] |fail| pw2052 [lorem-dev] ipsum: Dolor &amp; sit.</H1>
    <B>robot at robot.example.org</B>
    <A HREF="mailto:build%40l.example.org?Subject=Re%3A&In-Reply-To=%3C1.abc%40r.example.org%3E"
       TITLE="[lorem-build] |fail| pw2052">robot at robot.example.org
       </A><BR>
    <I>Mon Mar  2 10:00:00 UTC 2026</I>
    <HR>
<!--beginarticle-->
<PRE>Test-Label: lorem-robot: Build and Test
Test-Status: fail
<A HREF="https://pw.example.org/patch/2052/">https://pw.example.org/patch/2052/</A>

_lorem build: failed_
Build URL: <A HREF="https://ci.example.org/runs/1">https://ci.example.org/runs/1</A>
a &lt;b&gt; c
</PRE>

<!--endarticle-->
    <HR>
</BODY></HTML>`

func TestParseMessagePage(t *testing.T) {
	m, err := ParseMessagePage([]byte(testArticleHTML))
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "[lorem-build] |fail| pw2052 [lorem-dev] ipsum: Dolor & sit." {
		t.Errorf("Subject = %q", m.Subject)
	}
	if m.MessageID != "<1.abc@r.example.org>" {
		t.Errorf("MessageID = %q", m.MessageID)
	}
	if m.Date != "Mon Mar  2 10:00:00 UTC 2026" {
		t.Errorf("Date = %q", m.Date)
	}
	wantStart := "Test-Label: lorem-robot: Build and Test\nTest-Status: fail\n" +
		"https://pw.example.org/patch/2052/\n"
	if !strings.HasPrefix(m.Body, wantStart) || !strings.Contains(m.Body, "a <b> c") {
		t.Errorf("Body = %q", m.Body)
	}
	if len(m.Links) != 2 || m.Links[0] != "https://pw.example.org/patch/2052/" {
		t.Errorf("Links = %q", m.Links)
	}
}

func TestParseMessagePageNoArticle(t *testing.T) {
	if _, err := ParseMessagePage([]byte("<html>nothing</html>")); err == nil {
		t.Error("expected error")
	}
}

func TestFetchMessage(t *testing.T) {
	p, srv := testPipermail(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testArticleHTML))
	})
	u := p.MessageURL("2026-March", 60006)
	if u != srv.URL+"/pipermail/dev/2026-March/060006.html" {
		t.Errorf("MessageURL = %q", u)
	}
	m, err := p.FetchMessage(context.Background(), u)
	if err != nil || m.URL != u {
		t.Fatalf("m=%v err=%v", m, err)
	}
}

func TestFilterNewMessages(t *testing.T) {
	msgs := []ArchiveMessage{
		{Number: 100, Subject: "Old"},
		{Number: 200, Subject: "Also old"},
		{Number: 300, Subject: "New one"},
		{Number: 400, Subject: "Newer"},
	}

	filtered := FilterNewMessages(msgs, 200)
	if len(filtered) != 2 {
		t.Fatalf("len = %d, want 2", len(filtered))
	}
	if filtered[0].Number != 300 {
		t.Errorf("[0].Number = %d", filtered[0].Number)
	}
	if filtered[1].Number != 400 {
		t.Errorf("[1].Number = %d", filtered[1].Number)
	}
}

func TestFilterNewMessages_AllNew(t *testing.T) {
	msgs := []ArchiveMessage{
		{Number: 100, Subject: "A"},
		{Number: 200, Subject: "B"},
	}
	filtered := FilterNewMessages(msgs, 0)
	if len(filtered) != 2 {
		t.Errorf("len = %d, want 2", len(filtered))
	}
}

func TestFilterNewMessages_NoneNew(t *testing.T) {
	msgs := []ArchiveMessage{
		{Number: 100, Subject: "A"},
	}
	filtered := FilterNewMessages(msgs, 500)
	if len(filtered) != 0 {
		t.Errorf("len = %d, want 0", len(filtered))
	}
}

func TestExtractVersion(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    string
	}{
		{"simple v2", "[PATCH v2] Fix bug", "v2"},
		{"comma separated", "[dev,v3,1/3] Fix bug", "v3"},
		{"no version", "[PATCH] Fix bug", ""},
		{"no brackets", "Fix bug", ""},
		{"Re prefix", "Re: [PATCH v2] Fix bug", "v2"},
		{"multiple Re", "Re: Re: [dev,v2] Fix", "v2"},
		{"case insensitive", "[PATCH V10] Fix", "v10"},
		{"high version", "[PATCH v123] Fix", "v123"},
		{"version in second bracket",
			"[resend] [PATCH v2] Fix", "v2"},
		{"version in third bracket",
			"[RFC] [resend] [PATCH v4] Fix", "v4"},
		{"v1 explicit", "[PATCH v1] Fix bug", "v1"},
		{"space before version", "[PATCH v2] Fix", "v2"},
		{"PATCHv2 no space", "[PATCHv2] Fix", "v2"},
		{"was suffix ignored",
			"Re: [PATCH v3] New (was: Old)", "v3"},
		{"was with brackets",
			"Re: [PATCH v3] New (was: [PATCH v2] Old)", "v3"},
		{"version past non-bracket text ignored",
			"Re: New topic (was: [PATCH v2] Fix)", ""},
		{"empty subject", "", ""},
		{"only Re", "Re:", ""},
		{"brackets no patch", "[ovs-dev] Discussion", ""},
		{"chinese reply", "回复: [PATCH v2] Fix", "v2"},
		{"german reply", "AW: [PATCH v3] Fix", "v3"},
		{"swedish reply", "SV: [PATCH v4] Fix", "v4"},
		{"forwarded reply", "Fwd: Re: [PATCH v2] Fix", "v2"},
		{"unknown prefix", "XYZ: [PATCH v4] Fix", "v4"},
		{"nested forward",
			"转发: 回复: [PATCH v5] Fix", "v5"},
		{"prefix with brackets",
			"[Re] [PATCH v2] Fix", "v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractVersion(tt.subject)
			if got != tt.want {
				t.Errorf("extractVersion(%q) = %q, want %q",
					tt.subject, got, tt.want)
			}
		})
	}
}

func TestVersionsMatch(t *testing.T) {
	tests := []struct {
		name     string
		msgVer   string
		patchVer string
		want     bool
	}{
		{"same version", "v2", "v2", true},
		{"different version", "v2", "v3", false},
		{"v1 matches empty", "v1", "", true},
		{"empty matches v1", "", "v1", true},
		{"both empty", "", "", true},
		{"v2 does not match empty", "v2", "", false},
		{"empty does not match v2", "", "v2", false},
		{"same v1", "v1", "v1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := versionsMatch(tt.msgVer, tt.patchVer)
			if got != tt.want {
				t.Errorf("versionsMatch(%q, %q) = %v, want %v",
					tt.msgVer, tt.patchVer, got, tt.want)
			}
		})
	}
}

func TestMatchPatchSubjects_VersionFiltering(t *testing.T) {
	msgs := []ArchiveMessage{
		{Number: 200,
			Subject: "Re: [dev] [PATCH v2] Fix the widget"},
	}
	patchNames := map[int]string{
		1000: "[dev] Fix the widget",    // v1 (implicit)
		1001: "[dev,v2] Fix the widget", // v2
		1002: "[dev,v3] Fix the widget", // v3
	}
	ids := MatchPatchSubjects(msgs, patchNames)
	got := map[int]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if got[1000] {
		t.Error("v1 patch should NOT match v2 reply")
	}
	if !got[1001] {
		t.Error("v2 patch should match v2 reply")
	}
	if got[1002] {
		t.Error("v3 patch should NOT match v2 reply")
	}
}

func TestMatchPatchSubjects_NoVersionMatchesAll(t *testing.T) {
	msgs := []ArchiveMessage{
		{Number: 200,
			Subject: "Re: Fix the widget"},
	}
	patchNames := map[int]string{
		1000: "[dev] Fix the widget",
		1001: "[dev,v2] Fix the widget",
		1002: "[dev,v3] Fix the widget",
	}
	ids := MatchPatchSubjects(msgs, patchNames)
	got := map[int]bool{}
	for _, id := range ids {
		got[id] = true
	}
	// No version in reply → match all versions (avoid false negatives)
	if !got[1000] {
		t.Error("v1 should match (no version in reply)")
	}
	if !got[1001] {
		t.Error("v2 should match (no version in reply)")
	}
	if !got[1002] {
		t.Error("v3 should match (no version in reply)")
	}
}

func TestMatchPatchSubjects_V1MatchesUnversioned(t *testing.T) {
	msgs := []ArchiveMessage{
		{Number: 200,
			Subject: "Re: [dev] [PATCH v1] Fix the widget"},
	}
	patchNames := map[int]string{
		1000: "[dev] Fix the widget", // no explicit version = v1
	}
	ids := MatchPatchSubjects(msgs, patchNames)
	if len(ids) != 1 || ids[0] != 1000 {
		t.Errorf("v1 reply should match unversioned patch, got %v",
			ids)
	}
}

func TestMatchPatchSubjects_WasDoesNotCrossMatch(t *testing.T) {
	msgs := []ArchiveMessage{
		{Number: 200,
			Subject: "Re: [PATCH v3] New name " +
				"(was: [PATCH v2] Old name)"},
	}
	patchNames := map[int]string{
		1000: "[PATCH v2] Old name",
		1001: "[PATCH v3] New name",
	}
	ids := MatchPatchSubjects(msgs, patchNames)
	got := map[int]bool{}
	for _, id := range ids {
		got[id] = true
	}
	// Version is v3 (from main brackets), so only v3 should match
	if got[1000] {
		t.Error("v2 'Old name' should NOT match v3 reply")
	}
	if !got[1001] {
		t.Error("v3 'New name' should match v3 reply")
	}
}

func TestMatchPatchSubjects_WasOnlyVersion(t *testing.T) {
	// Subject changed — version is only in (was:) part, which is past
	// non-bracket text. Version extraction stops before reaching it,
	// so all matching patches are marked (safe fallback).
	msgs := []ArchiveMessage{
		{Number: 200,
			Subject: "Re: Discussion about the fix " +
				"(was: [PATCH v2] Fix the widget)"},
	}
	patchNames := map[int]string{
		1000: "[dev] Fix the widget",    // v1
		1001: "[dev,v2] Fix the widget", // v2
	}
	ids := MatchPatchSubjects(msgs, patchNames)
	got := map[int]bool{}
	for _, id := range ids {
		got[id] = true
	}
	// No version extracted → both versions match (no false negatives)
	if !got[1000] {
		t.Error("v1 should match (no version extracted)")
	}
	if !got[1001] {
		t.Error("v2 should match (no version extracted)")
	}
}

func TestMatchPatchSubjects_MixedVersionsAndUnrelated(t *testing.T) {
	msgs := []ArchiveMessage{
		{Number: 100,
			Subject: "Re: [ovs-dev] [PATCH] Fix bug"},
		{Number: 101,
			Subject: "[ovs-dev] Unrelated topic"},
		{Number: 102,
			Subject: "Re: [ovs-dev] [PATCH v2] Add feature"},
	}
	patchNames := map[int]string{
		1000: "[ovs-dev,v1] Fix bug",
		1001: "[ovs-dev] [PATCH v2] Add feature",
		1002: "[ovs-dev] Something else entirely",
	}
	ids := MatchPatchSubjects(msgs, patchNames)
	got := map[int]bool{}
	for _, id := range ids {
		got[id] = true
	}
	if !got[1000] {
		t.Error("patch 1000 should match (Fix bug, no version in reply → matches all)")
	}
	if !got[1001] {
		t.Error("patch 1001 should match (Add feature v2)")
	}
	if got[1002] {
		t.Error("patch 1002 should not match")
	}
}
