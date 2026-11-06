// Copyright 2026 Leadlight Authors
// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from leadlight sync/sync.go (commit 1f9d0e7), modified.

package pw

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type WalkOptions struct {
	Category string

	// Cursor is the ID of the last processed event.  Without a cursor
	// the walk stops at events older than Cutoff.
	Cursor int
	Cutoff time.Time

	// MaxPages bounds the walk; 0 means unlimited.
	MaxPages int
}

func ParseDate(s string) time.Time {
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// WalkEvents fetches events newest-first until it reaches the cursor,
// then hands new events to handle() oldest-first.  It does not use the
// `since` filter, which makes some instances fail with 500/502.
//
// The returned cursor is the ID of the last successfully handled event.
// A handler error stops the walk so that the event is retried later.
func (c *Client) WalkEvents(
	ctx context.Context, opts WalkOptions, handle func(Event) error,
) (int, error) {
	stop := func(ev Event) bool {
		if opts.Cursor > 0 {
			return ev.ID <= opts.Cursor
		}
		t := ParseDate(ev.Date)
		return !opts.Cutoff.IsZero() && !t.IsZero() && t.Before(opts.Cutoff)
	}

	base := c.BuildEventsURL(EventListParams{
		Project:  c.project,
		Order:    "-id",
		Category: opts.Category,
	})
	var pages [][]Event
	newest := 0
	for p := 1; ; p++ {
		page, err := c.GetEventsPage(ctx, fmt.Sprintf("%s&page=%d", base, p))
		if errors.Is(err, ErrInvalidPage) {
			break
		}
		if err != nil {
			return opts.Cursor, fmt.Errorf("events %s page %d: %w", opts.Category, p, err)
		}
		if len(page.Items) == 0 {
			break
		}
		if newest == 0 {
			newest = page.Items[0].ID
		}
		pages = append(pages, page.Items)
		stopped := false
		for _, ev := range page.Items {
			if stop(ev) {
				stopped = true
				break
			}
		}
		if stopped || (opts.MaxPages > 0 && p >= opts.MaxPages) {
			break
		}
	}

	cursor := opts.Cursor
	for i := len(pages) - 1; i >= 0; i-- {
		for j := len(pages[i]) - 1; j >= 0; j-- {
			ev := pages[i][j]
			if stop(ev) || ev.ID <= cursor {
				continue
			}
			if err := handle(ev); err != nil {
				return cursor, fmt.Errorf("event %d: %w", ev.ID, err)
			}
			cursor = ev.ID
		}
	}
	// A cursorless walk that found only old events still records where
	// the stream is, so the next walk is bounded by ID.
	if cursor == 0 && newest > 0 {
		cursor = newest
	}
	return cursor, nil
}
