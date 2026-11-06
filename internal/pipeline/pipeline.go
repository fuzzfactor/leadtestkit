// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"leadtestkit/internal/archive"
	"leadtestkit/internal/config"
	"leadtestkit/internal/gh"
	"leadtestkit/internal/logx"
	"leadtestkit/internal/mail"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/state"
)

type Patchwork interface {
	WalkEvents(ctx context.Context, opts pw.WalkOptions, handle func(pw.Event) error) (int, error)
	BuildSeriesURL(params pw.SeriesListParams) string
	GetSeriesPage(ctx context.Context, pageURL string) (*pw.PageResult[pw.Series], error)
	GetSeries(ctx context.Context, id int) (*pw.Series, error)
	GetPatch(ctx context.Context, id int) (*pw.PatchDetail, error)
	GetCover(ctx context.Context, id int) (*pw.CoverDetail, error)
	GetPatchComments(ctx context.Context, id int) ([]pw.Comment, error)
	GetPatchChecks(ctx context.Context, id int) ([]pw.Check, error)
	CreateCheck(ctx context.Context, patchID int, chk pw.NewCheck) error
	FindPatchesByMsgID(ctx context.Context, msgid string) ([]pw.Patch, error)
	PatchWebURL(id int) string
}

type GitHub interface {
	RefSHA(ctx context.Context, name string) (string, error)
	SetBranch(ctx context.Context, branch, sha string) error
	DeleteRef(ctx context.Context, name string) error
	MatchingRefs(ctx context.Context, prefix string) (map[string]string, error)
	RunsForSHA(ctx context.Context, sha string) ([]gh.WorkflowRun, error)
	DispatchRuns(ctx context.Context, branch string, since time.Time) ([]gh.WorkflowRun, error)
	ActiveRuns(ctx context.Context) ([]gh.WorkflowRun, error)
	Jobs(ctx context.Context, runID int64) ([]gh.Job, error)
	JobLog(ctx context.Context, jobID int64) (string, error)
	Rerun(ctx context.Context, runID int64, failedOnly bool) error
	Dispatch(ctx context.Context, file, ref string, inputs map[string]string) error
	WorkflowFiles(ctx context.Context, ref string) ([]string, error)
	File(ctx context.Context, path, ref string) ([]byte, error)
	TokenExpiry() string
}

type Archive interface {
	FetchMonth(ctx context.Context, year int, month time.Month) ([]archive.ArchiveMessage, error)
	FetchMessage(ctx context.Context, msgURL string) (*archive.Message, error)
	MessageURL(month string, number int) string
}

type WorkRepo interface {
	Resolve(ctx context.Context, url, branch string) (string, error)
	ResolveTestRef(ctx context.Context, ref string) (string, error)
	ReadFile(ctx context.Context, rev, path string) ([]byte, error)
	Apply(ctx context.Context, base string, mboxes [][]byte) ([]string, int, string, error)
	PushRefs(ctx context.Context, refs map[string]string) error
	TestRefs(ctx context.Context, refs []string) (map[string]string, error)
	Parent(ctx context.Context, sha string) (string, error)
	ChangedFiles(ctx context.Context, from, to string) ([]string, error)
}

type Pipeline struct {
	Cfg     *config.Config
	Store   *state.Store
	PW      Patchwork
	GH      GitHub
	Reports Archive
	Dev     Archive
	Repo    WorkRepo
	Mail    mail.Sender
	Now     func() time.Time
	Log     *slog.Logger

	// Checkpoint saves and publishes the state.  It may replace Store
	// when the remote state changed.
	Checkpoint func(ctx context.Context, msg string) error

	Stats Stats

	steps         int
	customFiles   []string
	customLoaded  bool
	expiryWarned  bool
	safetyNetDone bool
	gcDone        bool
}

type Stats struct {
	Discovered int
	Applied    int
	Pushed     int
	Dispatched int
	Emails     int
	Checks     int
	Ingested   int
	Rechecks   int
	Finished   int
	Errors     []string
}

func (s *Stats) Summary() string {
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(s.Discovered, "discovered")
	add(s.Applied, "applied")
	add(s.Pushed, "pushed")
	add(s.Dispatched, "dispatched")
	add(s.Emails, "emails")
	add(s.Checks, "checks")
	add(s.Ingested, "ingested")
	add(s.Rechecks, "rechecks")
	add(s.Finished, "finished")
	if len(s.Errors) > 0 {
		parts = append(parts, fmt.Sprintf("%d errors", len(s.Errors)))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, ", ")
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p *Pipeline) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Pipeline) errorf(format string, args ...any) {
	msg := logx.Redact(fmt.Sprintf(format, args...))
	p.Stats.Errors = append(p.Stats.Errors, msg)
	p.log().Error(msg)
	logx.Annotate("error", "%s", msg)
}

func (p *Pipeline) checkpoint(ctx context.Context, stage string) {
	if p.Checkpoint == nil {
		return
	}
	if err := p.Checkpoint(ctx, stage); err != nil {
		p.errorf("checkpoint after %s: %v", stage, err)
	}
}

func (p *Pipeline) Step(ctx context.Context, final bool) {
	p.steps++
	stages := []struct {
		name string
		fn   func(context.Context)
	}{
		{"manual", p.applyManual},
		{"discover", p.discover},
		{"qualify", p.qualify},
		{"rechecks", p.rechecks},
		{"monitor", p.monitor},
		{"schedule", p.schedule},
		{"report", p.report},
		{"ingest", p.ingest},
		{"finish", p.finish},
		{"gc", p.gc},
	}
	for _, s := range stages {
		if ctx.Err() != nil {
			return
		}
		// Pushing new work late in a run is pointless; the next run
		// picks it up with fresh data.
		if final && s.name == "schedule" {
			continue
		}
		s.fn(ctx)
		// Emails cannot be taken back, so their state is saved right
		// away; everything else is idempotent and saved once per step.
		if s.name == "report" {
			p.checkpoint(ctx, s.name)
		}
	}
	p.checkpoint(ctx, "step")
	p.warnTokenExpiry()
}

func (p *Pipeline) warnTokenExpiry() {
	if p.expiryWarned || p.GH == nil {
		return
	}
	exp := p.GH.TokenExpiry()
	if exp == "" {
		return
	}
	t, err := time.Parse("2006-01-02 15:04:05 MST", exp)
	if err != nil {
		return
	}
	if t.Sub(p.now()) < 14*24*time.Hour {
		p.expiryWarned = true
		p.log().Warn("test repository token expires soon", "expires", exp)
		logx.Annotate("warning", "LEADTEST_GITHUB_TOKEN expires on %s", exp)
	}
}

// applyManual handles flags set by humans in the state.
func (p *Pipeline) applyManual(ctx context.Context) {
	now := p.now()
	for _, s := range p.Store.Queue {
		if s.Skip != "" && s.Active() {
			p.log().Info("series skipped manually", "series", s.ID, "reason", s.Skip)
			s.Finish(state.SeriesSkipped, "manual: "+s.Skip, now)
		}
		if s.Retry {
			s.Retry = false
			s.Failure = nil
			p.log().Info("series retry requested", "series", s.ID)
			if s.Status == state.SeriesError || s.Status == state.SeriesSkipped ||
				s.Status == state.SeriesDone {
				resetSeries(s)
			}
		}
		for _, pt := range s.Patches {
			if pt.Retry {
				pt.Retry = false
				pt.Failure = nil
				for _, r := range pt.Runs {
					r.Failure = nil
				}
			}
		}
	}
}

func resetSeries(s *state.Series) {
	s.Status = state.SeriesQueued
	s.Reapply = true
	s.Reason = ""
	s.Skip = ""
	s.Started = ""
	s.Finished = ""
	s.Base = nil
	s.Failure = nil
	for _, pt := range s.Patches {
		pt.Status = state.PatchPending
		pt.SHA = ""
		pt.Pushed = ""
		pt.Failure = nil
		pt.Runs = nil
		pt.Recheck = false
	}
}

func (p *Pipeline) fail(f **state.Failure, op string, err error) bool {
	return state.RecordFailure(f, op, err, p.now(), p.Cfg.Limits.RetryDeadline.D())
}

func (p *Pipeline) branchURL(branch string) string {
	return fmt.Sprintf("https://%s/%s/tree/%s", p.Cfg.TestRepo.GitHost, p.Cfg.TestRepo.Repo, branch)
}

func (p *Pipeline) stagingRef(s *state.Series) string {
	return fmt.Sprintf("%sseries_%d", p.Cfg.TestRepo.StagingRef, s.ID)
}

func (p *Pipeline) patchRef(pt *state.Patch) string {
	return fmt.Sprintf("%spatch_%d", p.Cfg.TestRepo.StagingRef, pt.ID)
}

func (p *Pipeline) known() (map[int]bool, error) {
	window := max(p.Cfg.Limits.HistoryWindow.D(), p.Cfg.Limits.SeriesMaxAge.D()) + 24*time.Hour
	list, err := p.Store.Recent(p.now(), window)
	if err != nil {
		return nil, err
	}
	ids := map[int]bool{}
	for _, s := range list {
		ids[s.ID] = true
	}
	return ids, nil
}

func sortedPatches(s *state.Series) []*state.Patch {
	ps := append([]*state.Patch(nil), s.Patches...)
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Number < ps[j].Number })
	return ps
}

func authorKey(s *state.Series) string {
	if s.Submitter.Email != "" {
		return strings.ToLower(s.Submitter.Email)
	}
	return fmt.Sprintf("series-%d", s.ID)
}
