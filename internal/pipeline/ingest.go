// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"errors"
	"fmt"
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

var errSkip = errors.New("skip")

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
func (p *Pipeline) newMessages(ctx context.Context, a Archive,
	cursor string) ([]monthMessages, string, bool) {
	c := p.Store.Cursor(cursor)
	now := p.now()
	first := c.LastMonth == ""
	if first {
		p.infof("First scan of this archive: recording existing messages as seen, " +
			"not processing historical messages.")
	}
	var out []monthMessages
	for _, m := range scanMonths(c, now, p.Cfg.Archive.LookbackMonths) {
		p.infof("Reading archive index for %s.", m.Format("2006-01"))
		msgs, err := a.FetchMonth(ctx, m.Year(), m.Month())
		if err != nil {
			p.warnf("Cannot read archive index for %s: %v", m.Format("2006-01"), err)
			p.archiveRead(cursor, c, err)
			return out, m.Format("2006-01"), false
		}
		key := archive.MonthKey(m.Year(), m.Month())
		if first {
			c.Months[key] = max(c.Months[key], maxNumber(msgs))
			p.infof("Archive %s: marked %d messages as seen through message %d.",
				key, len(msgs), c.Months[key])
			continue
		}
		fresh := archive.FilterNewMessages(msgs, c.Months[key])
		p.infof("Archive %s: %d messages, %d new after saved message %d.",
			key, len(msgs), len(fresh), c.Months[key])
		sort.Slice(fresh, func(i, j int) bool { return fresh[i].Number < fresh[j].Number })
		out = append(out, monthMessages{key: key, msgs: fresh})
	}
	p.archiveRead(cursor, c, nil)
	return out, now.Format("2006-01"), true
}

func (p *Pipeline) ingest(ctx context.Context) {
	if !p.Cfg.Archive.Ingest {
		p.infof("Not ingesting reports: archive ingestion is disabled.")
		return
	}
	if p.Reports == nil {
		p.infof("Not ingesting reports: no reports archive is configured.")
		return
	}
	if p.reportsScanned {
		p.infof("Not rescanning reports: the archive was already read in this invocation.")
		return
	}
	if p.checksSkipped("reading archived reports") {
		return
	}
	p.infof("Scanning the reports archive; at most %d messages will be processed this cycle.",
		p.Cfg.Archive.MaxMessages)
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
		if budget <= 0 {
			p.infof("Stopping report retries: all %d message slots of this pass "+
				"are used.",
				p.Cfg.Archive.MaxMessages)
			return
		}
		if ctx.Err() != nil {
			p.infof("Stopping report retries: %v.", ctx.Err())
			return
		}
		if !f.Due(now) {
			p.infof("Waiting until %s to retry reading the report %s", f.Next, u)
			continue
		}
		if p.checksSkipped("reading archived reports") {
			return
		}
		budget--
		p.infof("Retrying the report %s", u)
		err := p.ingestMessage(ctx, u)
		if err == nil || errors.Is(err, errSkip) {
			delete(c.Retry, u)
			continue
		}
		if p.reportFailed(&f, err) {
			delete(c.Retry, u)
			p.errorf("giving up on the report %s after errors: %v", u, err)
			continue
		}
		c.Retry[u] = f
		p.logFailure(f, false)
	}

	months, next, ok := p.newMessages(ctx, p.Reports, reportsCursor)
	p.reportsScanned = ok
	for _, mm := range months {
		for _, m := range mm.msgs {
			if budget <= 0 || ctx.Err() != nil {
				why := fmt.Sprintf("all %d message slots of this pass are used",
					p.Cfg.Archive.MaxMessages)
				if budget > 0 {
					why = ctx.Err().Error()
				}
				p.infof("Stopping ingestion in %s: %s. The remaining messages "+
					"are read later.", mm.key, why)
				c.LastMonth = monthOfKey(mm.key)
				return
			}
			if p.checksSkipped("reading archived reports") {
				// The rest is read in the next cycle, not the next run.
				c.LastMonth = monthOfKey(mm.key)
				p.reportsScanned = false
				return
			}
			budget--
			u := p.Reports.MessageURL(mm.key, m.Number)
			if err := p.ingestMessage(ctx, u); err != nil && !errors.Is(err, errSkip) {
				var f *state.Failure
				p.reportFailed(&f, err)
				if c.Retry == nil {
					c.Retry = map[string]*state.Failure{}
				}
				c.Retry[u] = f
				p.warnf("Report %s: ingestion failed; will retry at %s: %v",
					u, f.Next, err)
			}
			c.Months[mm.key] = m.Number
		}
	}
	if ok || c.LastMonth != "" {
		c.LastMonth = next
	}
	p.infof("Reports scan finished: used %d of %d message slots; %d reports await retry.",
		p.Cfg.Archive.MaxMessages-budget, p.Cfg.Archive.MaxMessages, len(c.Retry))
}

// reportFailed records a failure to ingest a report and reports whether
// its deadline passed.  Reports that wait for an outage of Patchwork
// are retried at least every outageProbeInterval: they may be what
// tells that Patchwork accepts checks again.
func (p *Pipeline) reportFailed(f **state.Failure, err error) bool {
	now := p.now()
	kind := p.reportOutage(err)
	deadline := archiveTimeout
	if kind != "" {
		deadline = 0
	} else if *f != nil && (*f).Outage != "" {
		// The time waited for the outage does not count.
		(*f).First, (*f).Attempts = state.FormatTime(now), 0
	}
	done := state.RecordFailure(f, "ingest", err, now, deadline)
	(*f).Outage, (*f).Cause = kind, p.outageOf(err)
	if next := now.Add(outageProbeInterval); kind == state.OutagePatchwork &&
		state.ParseTime((*f).Next).After(next) {
		(*f).Next = state.FormatTime(next)
	}
	return done
}

// reportOutage tells the outage that a report waits for after an error,
// if any.  Reports wait without a deadline then.
func (p *Pipeline) reportOutage(err error) string {
	if kind := p.waitsFor(p.outageOf(err)); kind == state.OutagePatchwork {
		return kind
	}
	if kind := state.OutageArchive(reportsCursor); p.outage(kind) != nil {
		return kind
	}
	return ""
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
	p.infof("Reading the archived report %s", msgURL)
	m, err := p.Reports.FetchMessage(ctx, msgURL)
	if err != nil {
		return err
	}
	parsed, err := report.Parse(m.Subject, m.Body, m.Links, p.pwHost(), p.Cfg.Mode.TestEmail())
	if err != nil {
		p.infof("Ignoring the archive message %s as not a report: %v", msgURL, err)
		return errSkip
	}
	own := report.IsOwnMessageID(m.MessageID, p.Cfg.Report.MessageIDDomain)
	if parsed.Test && !own {
		p.infof("Ignoring the test report %s sent by another bot instance.", msgURL)
		return errSkip
	}
	// Pending checks of our own runs are posted directly with a link to
	// the test branch.
	if own && parsed.Status == state.ResultPending && p.Cfg.Report.PendingCheck() {
		p.infof("Ignoring our pending report %s: pending checks are posted directly.",
			msgURL)
		return errSkip
	}
	patchID := parsed.PatchID
	if patchID == 0 {
		found, err := p.pw().FindPatchesByMsgID(ctx, parsed.PatchMsgID)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			p.infof("Ignoring report %s: no Patchwork patch matches Message-ID %s.",
				msgURL, parsed.PatchMsgID)
			return errSkip
		}
		patchID = found[0].ID
	}
	ok, name, err := p.ourPatch(ctx, patchID)
	if err != nil {
		return err
	}
	if name == "" {
		name = m.Subject
	}
	if !ok {
		p.infof("Ignoring report %s for patch %d (%s): the patch is not in our project.",
			msgURL, patchID, name)
		return errSkip
	}
	checks, err := p.pw().GetPatchChecks(ctx, patchID)
	if err != nil {
		return err
	}
	for _, ch := range checks {
		if ch.TargetURL != nil && stripScheme(*ch.TargetURL) == stripScheme(msgURL) {
			p.infof("Patch %d (%s): report %s already has a check; not posting twice.",
				patchID, name, msgURL)
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
		p.noteCheck(patchID, "check-logged",
			fmt.Sprintf("Would post %s check %s from %s", chk.State, chk.Context,
				msgURL))
		p.infof("Patch %d (%s): would post %s check %s (%s), checks mode is %s, "+
			"linking to %s", patchID, name, chk.State, chk.Context,
			chk.Description, p.Cfg.Mode.Checks, chk.TargetURL)
		return nil
	}
	p.infof("Patch %d (%s): posting %s check %s linking to the archived report %s",
		patchID, name, chk.State, chk.Context, msgURL)
	if err := p.pw().CreateCheck(ctx, patchID, chk); err != nil {
		// Refused tokens and checks wait for the outage they start.
		if code := httpx.StatusCode(err); code >= 400 && code < 500 &&
			code != http.StatusTooManyRequests && code != http.StatusUnauthorized &&
			!isCheckRefusal(err) {
			p.noteCheck(patchID, "check-rejected",
				fmt.Sprintf("Check %s from %s rejected: %v", chk.Context, msgURL,
					err))
			p.warnf("Patch %d (%s): check %s rejected; not retrying this report: %v",
				patchID, name, chk.Context, err)
			return errSkip
		}
		p.noteCheck(patchID, "check-failed",
			fmt.Sprintf("Check %s from %s failed: %v", chk.Context, msgURL, err))
		return err
	}
	p.Stats.Checks++
	p.noteCheck(patchID, "check-posted",
		fmt.Sprintf("%s check %s from %s", chk.State, chk.Context, msgURL))
	p.infof("Patch %d (%s): posted %s check %s.", patchID, name, chk.State, chk.Context)
	return nil
}

// ourPatch reports whether a patch belongs to the configured project.
// Reports lists may be shared between projects with global patch IDs.
func (p *Pipeline) ourPatch(ctx context.Context, id int) (bool, string, error) {
	recent, err := p.Store.Recent(p.now(), p.Cfg.Limits.HistoryWindow.D())
	if err != nil {
		return false, "", err
	}
	if _, pt := state.FindPatch(recent, id); pt != nil {
		return true, pt.Name, nil
	}
	d, err := p.pw().GetPatch(ctx, id)
	if httpx.IsNotFound(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return p.inProject(d), d.Name, nil
}

func (p *Pipeline) inProject(d *pw.PatchDetail) bool {
	proj := p.Cfg.Patchwork.Project
	return strings.EqualFold(d.Project.LinkName, proj) ||
		strings.EqualFold(d.Project.Name, proj) ||
		strings.EqualFold(d.Project.ListID, proj)
}
