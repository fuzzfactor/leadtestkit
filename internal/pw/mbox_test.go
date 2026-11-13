// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pw

import (
	"strings"
	"testing"
)

func TestUpgradeHTTP(t *testing.T) {
	tests := []struct{ in, want string }{
		{"http://pw.example.com/patch/1/", "https://pw.example.com/patch/1/"},
		{"https://pw.example.com/patch/1/", "https://pw.example.com/patch/1/"},
		{"", ""},
		{"ftp://example.com", "ftp://example.com"},
	}
	for _, tt := range tests {
		if got := UpgradeHTTP(tt.in); got != tt.want {
			t.Errorf("UpgradeHTTP(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestAuthor(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]any
		want    string
	}{
		{"plain from", map[string]any{"From": "Lorem Ipsum <lorem@example.org>",
			"Reply-To": "dev@lists.example.org"}, `"Lorem Ipsum" <lorem@example.org>`},
		{"dmarc rewritten", map[string]any{
			"From":     "Lorem Ipsum via dev <dev@lists.example.org>",
			"Reply-To": "Lorem Ipsum <lorem@example.org>"},
			`"Lorem Ipsum" <lorem@example.org>`},
		{"dmarc rewritten, address lost", map[string]any{
			"From":     "Lorem Ipsum via dev <dev@lists.example.org>",
			"Reply-To": "dev@lists.example.org"},
			"Lorem Ipsum via dev <dev@lists.example.org>"},
		{"encoded", map[string]any{"From": "=?utf-8?q?D=C3=B6lor?= <dolor@example.org>"},
			"=?utf-8?q?D=C3=B6lor?= <dolor@example.org>"},
		{"list value", map[string]any{"from": []any{"Sit Amet <sit@example.org>"}},
			`"Sit Amet" <sit@example.org>`},
		{"fallback to submitter", map[string]any{}, `"Consectetur" <cons@example.org>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &PatchDetail{Headers: tt.headers}
			p.Submitter = Person{Name: "Consectetur", Email: "cons@example.org"}
			if got := Author(p); got != tt.want {
				t.Errorf("Author = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPatchNumber(t *testing.T) {
	tests := map[string]int{
		"[PATCH v2 3/5] lorem":        3,
		"[ovs-dev,v2,12/15] ipsum":    12,
		"[ovs-dev] [PATCH 1/2] dolor": 1,
		"sit amet":                    0,
	}
	for name, want := range tests {
		if got := PatchNumber(name); got != want {
			t.Errorf("PatchNumber(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestBuildMbox(t *testing.T) {
	p := &PatchDetail{
		Headers: map[string]any{
			"From":     "Lorem Ipsum via dev <dev@lists.example.org>",
			"Reply-To": "Lörem Ipsum <lorem@example.org>",
			"Subject":  "[PATCH v2 1/2] dolor: Fix\n sit amet ü.",
			"Date":     "Mon, 2 Mar 2026 10:00:00 +0100",
		},
		Content: "Body line.\nFrom here on.\n\n" +
			"Signed-off-by: Lorem Ipsum <lorem@example.org>\n",
		Diff: "diff --git a/f b/f\n--- a/f\n+++ b/f\n@@ -1 +1 @@\n-a\n+b\n",
	}
	p.ID = 5
	p.MsgID = "<1@example.org>"
	out, err := BuildMbox(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"From: =?utf-8?q?L=C3=B6rem_Ipsum?= <lorem@example.org>\n",
		"Subject: =?utf-8?q?",
		"Message-Id: <1@example.org>\n",
		"Date: Mon, 2 Mar 2026 10:00:00 +0100\n",
		"\n\nBody line.\nFrom here on.\n",
		"---\ndiff --git a/f b/f\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("mbox missing %q:\n%s", want, s)
		}
	}
	if strings.HasPrefix(s, "From ") {
		t.Errorf("mbox must not start with an mbox separator")
	}
}

func TestBuildMboxNoDiff(t *testing.T) {
	if _, err := BuildMbox(&PatchDetail{}); err == nil {
		t.Error("expected error for patch without diff")
	}
}
