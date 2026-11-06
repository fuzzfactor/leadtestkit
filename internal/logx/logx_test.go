// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package logx

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	t.Cleanup(resetSecretsForTest)
	AddSecret("lorem-secret-token")
	AddSecret("ab")
	got := Redact("Authorization: Token lorem-secret-token ab")
	if got != "Authorization: Token [REDACTED] ab" {
		t.Errorf("got %q", got)
	}
}

func TestLoggerRedactsMessageAndAttrs(t *testing.T) {
	t.Cleanup(resetSecretsForTest)
	AddSecret("ipsum-dolor-pass")
	var buf bytes.Buffer
	l := New(&buf, false)
	l.Info("login ipsum-dolor-pass", "pw", "ipsum-dolor-pass",
		"err", errors.New("bad ipsum-dolor-pass"))
	l.With("x", "ipsum-dolor-pass").Info("with")
	if strings.Contains(buf.String(), "ipsum-dolor-pass") {
		t.Errorf("secret leaked: %s", buf.String())
	}
	if strings.Count(buf.String(), "[REDACTED]") != 4 {
		t.Errorf("unexpected output: %s", buf.String())
	}
}
