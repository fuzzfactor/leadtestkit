// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

var (
	secretsMu sync.RWMutex
	secrets   []string
)

// AddSecret registers a value that must never appear in logs or in
// error text persisted to the state.
func AddSecret(s string) {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return
	}
	secretsMu.Lock()
	defer secretsMu.Unlock()
	for _, x := range secrets {
		if x == s {
			return
		}
	}
	secrets = append(secrets, s)
}

func Redact(s string) string {
	secretsMu.RLock()
	defer secretsMu.RUnlock()
	for _, x := range secrets {
		s = strings.ReplaceAll(s, x, "[REDACTED]")
	}
	return s
}

func resetSecretsForTest() {
	secretsMu.Lock()
	secrets = nil
	secretsMu.Unlock()
}

type redactHandler struct {
	h slog.Handler
}

func (r redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return r.h.Enabled(ctx, l)
}

func (r redactHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return r.h.Handle(ctx, out)
}

func (r redactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a)
	}
	return redactHandler{r.h.WithAttrs(red)}
}

func (r redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{r.h.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if a.Equal(slog.Attr{}) {
		return a
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindGroup:
		group := v.Group()
		red := make([]any, len(group))
		for i, g := range group {
			red[i] = redactAttr(g)
		}
		return slog.Group(a.Key, red...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, Redact(err.Error()))
		}
		return slog.String(a.Key, Redact(fmt.Sprint(v.Any())))
	}
	return a
}

func New(w io.Writer, debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	h := &humanHandler{output: &logOutput{w: w}, level: level}
	return slog.New(redactHandler{h})
}

func Setup(debug bool) *slog.Logger {
	l := New(os.Stderr, debug)
	slog.SetDefault(l)
	return l
}

// Annotate prints a GitHub Actions workflow command so the message
// shows up in the run summary.  Level is "warning", "error" or "notice".
func Annotate(level, format string, args ...any) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	msg := Redact(fmt.Sprintf(format, args...))
	msg = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(msg)
	fmt.Fprintf(os.Stdout, "::%s::%s\n", level, msg)
}
