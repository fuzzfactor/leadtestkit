// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestRunURL(t *testing.T) {
	t.Setenv("GITHUB_SERVER_URL", "https://github.example.org")
	t.Setenv("GITHUB_REPOSITORY", "lorem/bot")
	t.Setenv("GITHUB_RUN_ID", "42")
	t.Setenv("GITHUB_RUN_ATTEMPT", "2")
	want := "https://github.example.org/lorem/bot/actions/runs/42/attempts/2"
	if got := runURL(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	t.Setenv("GITHUB_RUN_ATTEMPT", "")
	if got := runURL(); got != "https://github.example.org/lorem/bot/actions/runs/42" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("GITHUB_RUN_ID", "")
	if got := runURL(); got != "" {
		t.Fatalf("local run has a URL: %q", got)
	}
}
