// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"unicode"
)

type logOutput struct {
	sync.Mutex
	w io.Writer
}

type humanHandler struct {
	output *logOutput
	level  slog.Level
	groups []string
	attrs  string
}

func (h *humanHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *humanHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	if !r.Time.IsZero() {
		b.WriteString(r.Time.UTC().Format("2006-01-02 15:04:05 UTC "))
	}
	fmt.Fprintf(&b, "%-5s %s\n", r.Level, readable(r.Message))
	b.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, h.groups, a)
		return true
	})
	h.output.Lock()
	defer h.output.Unlock()
	_, err := io.WriteString(h.output.w, Redact(b.String()))
	return err
}

func (h *humanHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	copy := *h
	var b strings.Builder
	b.WriteString(h.attrs)
	for _, a := range attrs {
		writeAttr(&b, h.groups, a)
	}
	copy.attrs = b.String()
	return &copy
}

func (h *humanHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	copy := *h
	copy.groups = append(append([]string(nil), h.groups...), name)
	return &copy
}

func writeAttr(b *strings.Builder, groups []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			groups = append(append([]string(nil), groups...), a.Key)
		}
		for _, child := range a.Value.Group() {
			writeAttr(b, groups, child)
		}
		return
	}
	labels := make([]string, 0, len(groups)+1)
	for _, name := range append(append([]string(nil), groups...), a.Key) {
		name = strings.ReplaceAll(name, "_", " ")
		if name == "err" {
			name = "error"
		}
		runes := []rune(name)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
		}
		labels = append(labels, readable(string(runes)))
	}
	fmt.Fprintf(b, "    %s: %s\n", strings.Join(labels, " / "), readable(a.Value.String()))
}

// Indent multiline messages and escape controls so remote text cannot forge log entries.
func readable(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString("\n    ")
		case r == '\t':
			b.WriteString("    ")
		case unicode.IsControl(r):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
