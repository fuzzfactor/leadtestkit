// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"leadtestkit/internal/archive"
	"leadtestkit/internal/httpx"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/report"
	"leadtestkit/internal/state"
)

const (
	reportsCursor  = "reports"
	devCursor      = "dev"
	maxCatchUp     = 12
	archiveTimeout = 7 * 24 * time.Hour
)

// errSkip marks a message that will never become a check.
var errSkip = errors.New("skip")

// scanMonths returns the months to scan, oldest first: everything since
// the last scan, and at least the lookback months.
func scanMonths(c *state.ArchiveCursor, now time.Time, lookback int) []time.Time {
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	start := cur.AddDate(0, -lookback, 0)
	if last, err := time.Parse("2006-01", c.LastMonth); err == nil && last.Before(start) {
		start = last
	}
	if limit := cur.AddDate(0, -maxCatchUp, 0); start.Before(limit) {
		start = limit
	}
	var out []time.Time
	for m := start; !m.After(cur); m = m.AddDate(0, 1, 0) {
		out = append(out, m)
	}
	return out
}

func maxNumber(msgs []archive.ArchiveMessage) int {
	n := 0
	for _, m := range msgs {
		n = max(n, m.Number)
	}
	return n
}

type monthMessages struct {
	key  string
	msgs []archive.ArchiveMessage
}

// newMessages lists messages after the cursor in each month.  On the
// first scan, existing messages are only recorded as seen.  It returns
// the month (YYYY-MM) from which the next scan has to start.
func (p *Pipeline) newMessages(ctx context.Context, a Archive, c *state.ArchiveCursor) ([]monthMessages, string, bool) {
	now := p.now()
	first := c.LastMonth == ""
	var out []monthMessages
	for _, m := range scanMonths(c, now, p.Cfg.Archive.LookbackMonths) {
		msgs, err := a.FetchMonth(ctx, m.Year(), m.Month())
		if err != nil {
			p.log().Warn("cannot read archive", "month", m.Format("2006-01"), "err", err)
			return out, m.Format("2006-01"), false
		}
		key := archive.MonthKey(m.Year(), m.Month())
		if first {
			c.Months[key] = max(c.Months[key], maxNumber(msgs))
			continue
		}
		fresh := archive.FilterNewMessages(msgs, c.Months[key])
		sort.Slice(fresh, func(i, j int) bool { return fresh[i].Number < fresh[j].Number })
		out = append(out, monthMessages{key: key, msgs: fresh})
	}
	return out, now.Format("2006-01"), true
}

// ingest posts reports found in the reports list archive as checks.
func (p *Pipeline) ingest(ctx context.Context) {
	if !p.Cfg.Archive.Ingest || p.Reports == nil {
		return
	}
	c := p.Store.Cursor(reportsCursor)
	budget := p.Cfg.Archive.MaxMessages
	now := p.now()

	urls := make([]string, 0, len(c.Retry))
	for u := range c.Retry {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	for _, u := range urls {
		f := c.Retry[u]
		if budget <= 0 || ctx.Err() != nil {
			return
		}
		if !f.Due(now) {
			continue
		}
		budget--
		err := p.ingestMessage(ctx, u)
		if err == nil || errors.Is(err, errSkip) {
			delete(c.Retry, u)
			continue
		}
		if state.RecordFailure(&f, "ingest", err, now, archiveTimeout) {
			delete(c.Retry, u)
			p.errorf("giving up on report %s: %v", u, err)
			continue
		}
		c.Retry[u] = f
	}

	months, next, ok := p.newMessages(ctx, p.Reports, c)
	for _, mm := range months {
		for _, m := range mm.msgs {
			if budget <= 0 || ctx.Err() != nil {
				c.LastMonth = monthOfKey(mm.key)
				return
			}
			budget--
			u := p.Reports.MessageURL(mm.key, m.Number)
			if err := p.ingestMessage(ctx, u); err != nil && !errors.Is(err, errSkip) {
				var f *state.Failure
				state.RecordFailure(&f, "ingest", err, now, archiveTimeout)
				if c.Retry == nil {
					c.Retry = map[string]*state.Failure{}
				}
				c.Retry[u] = f
				p.log().Warn("report ingest failed, will retry", "url", u, "err", err)
			}
			c.Months[mm.key] = m.Number
		}
	}
	if ok || c.LastMonth != "" {
		c.LastMonth = next
	}
}

func monthOfKey(key string) string {
	t, err := time.Parse("2006-January", key)
	if err != nil {
		return ""
	}
	return t.Format("2006-01")
}

func stripScheme(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	return strings.TrimRight(u, "/")
}

func (p *Pipeline) pwHost() string {
	u, err := url.Parse(p.Cfg.PatchworkWebURL())
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (p *Pipeline) ingestMessage(ctx context.Context, msgURL string) error {
	m, err := p.Reports.FetchMessage(ctx, msgURL)
	if err != nil {
		return err
	}
	parsed, err := report.Parse(m.Subject, m.Body, m.Links, p.pwHost(), p.Cfg.Mode.TestEmail())
	if err != nil {
		p.log().Debug("not a report", "url", msgURL, "reason", err)
		return errSkip
	}
	own := report.IsOwnMessageID(m.MessageID, p.Cfg.Report.MessageIDDomain)
	if parsed.Test && !own {
		return errSkip
	}
	// Pending checks of our own runs are posted directly with a link to
	// the test branch.
	if own && parsed.Status == state.ResultPending && p.Cfg.Report.PendingCheck {
		return errSkip
	}
	patchID := parsed.PatchID
	if patchID == 0 {
		found, err := p.PW.FindPatchesByMsgID(ctx, parsed.PatchMsgID)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			p.log().Info("report for unknown patch", "url", msgURL, "msgid", parsed.PatchMsgID)
			return errSkip
		}
		patchID = found[0].ID
	}
	if ok, err := p.ourPatch(ctx, patchID); err != nil {
		return err
	} else if !ok {
		p.log().Debug("report for another project", "url", msgURL, "patch", patchID)
		return errSkip
	}
	checks, err := p.PW.GetPatchChecks(ctx, patchID)
	if err != nil {
		return err
	}
	for _, ch := range checks {
		if ch.TargetURL != nil && stripScheme(*ch.TargetURL) == stripScheme(msgURL) {
			return errSkip
		}
	}
	chk := pw.NewCheck{
		State:       parsed.Status,
		TargetURL:   msgURL,
		Context:     report.Context(parsed.Label),
		Description: parsed.Description,
	}
	p.Stats.Ingested++
	if !p.Cfg.Mode.PostChecks() {
		p.log().Info("would post check", "patch", patchID, "state", chk.State, "context", chk.Context,
			"target_url", chk.TargetURL, "description", chk.Description)
		return nil
	}
	if err := p.PW.CreateCheck(ctx, patchID, chk); err != nil {
		if code := httpx.StatusCode(err); code >= 400 && code < 500 && code != http.StatusTooManyRequests {
			p.log().Warn("check rejected", "patch", patchID, "context", chk.Context, "err", err)
			return errSkip
		}
		return err
	}
	p.Stats.Checks++
	p.log().Info("check posted", "patch", patchID, "state", chk.State, "context", chk.Context)
	return nil
}

// ourPatch reports whether a patch belongs to the configured project.
// Reports lists may be shared between projects with global patch IDs.
func (p *Pipeline) ourPatch(ctx context.Context, id int) (bool, error) {
	recent, err := p.Store.Recent(p.now(), p.Cfg.Limits.HistoryWindow.D())
	if err != nil {
		return false, err
	}
	if _, pt := state.FindPatch(recent, id); pt != nil {
		return true, nil
	}
	d, err := p.PW.GetPatch(ctx, id)
	if httpx.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	proj := p.Cfg.Patchwork.Project
	return strings.EqualFold(d.Project.LinkName, proj) || strings.EqualFold(d.Project.Name, proj) ||
		strings.EqualFold(d.Project.ListID, proj), nil
}
