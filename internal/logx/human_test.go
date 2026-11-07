// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package logx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHumanFormat(t *testing.T) {
	var buf bytes.Buffer
	h := New(&buf, false).Handler()
	stamp := time.Date(2026, 11, 7, 12, 30, 0, 0, time.FixedZone("test", 3600))
	r := slog.NewRecord(stamp, slog.LevelInfo, "Series 7: waiting for CI capacity.", 0)
	r.AddAttrs(slog.Int("queued_jobs", 12), slog.Duration("delay", 3*time.Minute))
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	want := "2026-11-07 11:30:00 UTC INFO  Series 7: waiting for CI capacity.\n" +
		"    Queued jobs: 12\n    Delay: 3m0s\n"
	if got := buf.String(); got != want {
		t.Fatalf("log:\n%s\nwant:\n%s", got, want)
	}
}

func TestHumanGroupsAndBoundAttributes(t *testing.T) {
	var buf bytes.Buffer
	root := New(&buf, false)
	base := root.With("project", "lorem").WithGroup("request")
	left := base.With("method", "GET").WithGroup("response")
	right := base.With("method", "POST")
	left.Info("Finished.", slog.Group("", slog.Int("status", 200)), slog.Attr{})
	right.Info("Sending.", slog.Group("body", slog.Int("size", 12)))
	root.Info("Idle.")
	got := buf.String()
	for _, want := range []string{
		"    Project: lorem\n",
		"    Request / Method: GET\n",
		"    Request / Response / Status: 200\n",
		"    Request / Method: POST\n",
		"    Request / Body / Size: 12\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Count(got, "Project:") != 2 ||
		strings.Contains(got, "Request / Response / Method") {
		t.Errorf("attributes leaked across derived loggers:\n%s", got)
	}
	if strings.Contains(got, "Response / :") || strings.Contains(got, "<nil>") {
		t.Errorf("empty attribute was not ignored:\n%s", got)
	}
}

type logValue string

func (v logValue) LogValue() slog.Value {
	return slog.GroupValue(slog.String("token", string(v)))
}

func TestHumanRedactionAndUntrustedText(t *testing.T) {
	t.Cleanup(resetSecretsForTest)
	var buf bytes.Buffer
	l := New(&buf, false).With("saved", "late-secret")
	AddSecret("late-secret")
	l.Warn("remote says:\n::error::not an annotation\r\x1b[31m late-secret",
		slog.Any("credentials", logValue("late-secret")),
		"err", errors.New("bad late-secret\nsecond line"))
	got := buf.String()
	if strings.Contains(got, "late-secret") || strings.Count(got, "[REDACTED]") != 4 {
		t.Fatalf("redaction failed:\n%s", got)
	}
	for _, want := range []string{
		"\n    ::error::not an annotation\\u000d\\u001b[31m",
		"    Credentials / Token: [REDACTED]",
		"    Error: bad [REDACTED]\n    second line",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestHumanDebugLevel(t *testing.T) {
	for _, debug := range []bool{false, true} {
		var buf bytes.Buffer
		l := New(&buf, debug)
		l.Debug("Git details.")
		l.Info("Starting.")
		if strings.Contains(buf.String(), "Git details.") != debug {
			t.Errorf("debug %t: %s", debug, buf.String())
		}
		if !strings.Contains(buf.String(), "Starting.") {
			t.Errorf("info missing with debug %t", debug)
		}
	}
}

func TestHumanConcurrentRecords(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, false)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.With("worker", i).Info(fmt.Sprintf("Message %d.\nContinuation %d.", i, i))
		}(i)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 60 {
		t.Fatalf("got %d lines", len(lines))
	}
	for i := 0; i < len(lines); i += 3 {
		if !strings.Contains(lines[i], "INFO") ||
			!strings.HasPrefix(lines[i+1], "    Continuation ") ||
			!strings.HasPrefix(lines[i+2], "    Worker: ") {
			t.Fatalf("interleaved record: %q", lines[i:i+3])
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestHumanWriteError(t *testing.T) {
	r := slog.NewRecord(time.Time{}, slog.LevelError, "Failed.", 0)
	err := New(failingWriter{}, false).Handler().Handle(context.Background(), r)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("got %v", err)
	}
}
