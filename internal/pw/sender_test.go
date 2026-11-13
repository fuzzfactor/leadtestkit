// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pw

import "testing"

func TestSender(t *testing.T) {
	for _, tc := range []struct {
		what    string
		headers map[string]any
		name    string
		email   string
		ok      bool
	}{
		{"plain", map[string]any{"From": "Lorem Ipsum <lorem@example.org>"},
			"Lorem Ipsum", "lorem@example.org", true},
		{"encoded", map[string]any{
			"From": "=?utf-8?q?Ipsum=2C_L=C3=B6rem?= <lorem@example.org>"},
			"Ipsum, Lörem", "lorem@example.org", true},
		{"reply-to without a name", map[string]any{
			"From":     "Lorem via dev <dev@lists.example.org>",
			"Reply-To": "lorem@example.org"},
			"Lorem", "lorem@example.org", true},
		{"relay and list", map[string]any{
			"From": "Lorem Ipsum via B4 Relay via dev " +
				"<dev@lists.example.org>",
			"Reply-To":        "lorem@example.org",
			"X-Original-From": "Lorem Ipsum <lorem@example.org>",
			"Cc": "Lorem Ipsum via B4 Relay\n " +
				"<devnull+lorem.example.org@relay.example.org>"},
			"Lorem Ipsum", "lorem@example.org", true},
		{"relay only", map[string]any{
			"From":     "Lorem Ipsum via B4 Relay <devnull+lorem@relay.example.org>",
			"Reply-To": "lorem@example.org"},
			"Lorem Ipsum", "lorem@example.org", true},
		{"reply to the list, sender in cc", map[string]any{
			"From":     "'Lorem Ipsum' via dev <dev@lists.example.org>",
			"Reply-To": "dev@lists.example.org",
			"Cc":       "Dolor <dolor@example.org>, Lorem Ipsum <lorem@example.org>"},
			"Lorem Ipsum", "lorem@example.org", true},
		{"lost", map[string]any{
			"From":     "Lorem via dev <dev@lists.example.org>",
			"Reply-To": "dev@lists.example.org"},
			"Lorem", "", false},
	} {
		p := &PatchDetail{Headers: tc.headers}
		name, email, ok := Sender(p)
		if name != tc.name || email != tc.email || ok != tc.ok {
			t.Errorf("%s: got %q %q %v, want %q %q %v", tc.what, name, email, ok,
				tc.name, tc.email, tc.ok)
		}
	}
	p := &PatchDetail{}
	p.Submitter = Person{Name: "Lorem", Email: "lorem@example.org"}
	if name, email, ok := Sender(p); name != "Lorem" || email != "lorem@example.org" || !ok {
		t.Errorf("without headers: %q %q %v", name, email, ok)
	}
}
