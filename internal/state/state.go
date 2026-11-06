// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"time"
)

const SchemaVersion = 1

// Series statuses.
const (
	SeriesNew     = "new"
	SeriesQueued  = "queued"
	SeriesWaiting = "waiting"
	SeriesTesting = "testing"
	SeriesDone    = "done"
	SeriesSkipped = "skipped"
	SeriesError   = "error"
)

// Patch statuses.
const (
	PatchPending     = "pending"
	PatchApplied     = "applied"
	PatchApplyFailed = "apply-failed"
	PatchNotApplied  = "not-applied"
	PatchPushed      = "pushed"
	PatchDone        = "done"
	PatchSkipped     = "skipped"
)

// Run kinds.
const (
	RunApply    = "apply"
	RunPush     = "push"
	RunDispatch = "dispatch"
	RunMissing  = "missing"
)

// Normalized results, also used as Patchwork check states.
const (
	ResultSuccess = "success"
	ResultFail    = "fail"
	ResultWarning = "warning"
	ResultPending = "pending"
)

// Report step states.
const (
	StepPosted  = "posted"
	StepLogged  = "logged"
	StepSending = "sending"
	StepSent    = "sent"
	StepSkipped = "skipped"
	StepFailed  = "failed"
)

type Meta struct {
	Schema   int                       `json:"schema"`
	Events   map[string]int            `json:"events"`
	Archives map[string]*ArchiveCursor `json:"archives"`
	Stats    Stats                     `json:"stats"`
}

type ArchiveCursor struct {
	// LastMonth is the latest month (YYYY-MM) that was scanned.
	LastMonth string `json:"last_month,omitempty"`
	// Months maps pipermail month names to the highest message number
	// that was handled.
	Months map[string]int `json:"months"`
	// Retry holds messages that failed and will be read again.
	Retry map[string]*Failure `json:"retry,omitempty"`
}

type Stats struct {
	// JobsPerPatch is a moving average of jobs started per pushed patch.
	JobsPerPatch float64 `json:"jobs_per_patch,omitempty"`
}

type Person struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type Base struct {
	GitURL string `json:"git_url"`
	Branch string `json:"branch,omitempty"`
	SHA    string `json:"sha,omitempty"`
	// Why explains the choice, e.g. "subject: branch-3.4".
	Why string `json:"why,omitempty"`
}

type Series struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Version   int    `json:"version,omitempty"`
	Date      string `json:"date"`
	WebURL    string `json:"web_url,omitempty"`
	Submitter Person `json:"submitter"`
	Total     int    `json:"total"`
	CoverID   int    `json:"cover_id,omitempty"`

	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`

	// Hold, Skip and Retry are for humans editing the state.
	Hold  bool   `json:"hold,omitempty"`
	Skip  string `json:"skip,omitempty"`
	Retry bool   `json:"retry,omitempty"`

	DependsOn int    `json:"depends_on,omitempty"`
	Base      *Base  `json:"base,omitempty"`
	Branch    string `json:"branch,omitempty"`

	Discovered string `json:"discovered"`
	// Checked is when an incomplete series was last looked at.
	Checked  string `json:"checked,omitempty"`
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished,omitempty"`

	Failure *Failure `json:"failure,omitempty"`
	Patches []*Patch `json:"patches"`
}

type Patch struct {
	ID     int    `json:"id"`
	Number int    `json:"number"`
	Name   string `json:"name"`
	MsgID  string `json:"msgid"`
	Status string `json:"status"`
	SHA    string `json:"sha,omitempty"`
	Pushed string `json:"pushed,omitempty"`

	// CommentsSeen is the highest comment ID checked for rechecks.
	CommentsSeen int `json:"comments_seen,omitempty"`

	// Recheck requests a rerun of all runs, from a Recheck-request
	// comment or a human.
	Recheck bool     `json:"recheck,omitempty"`
	Retry   bool     `json:"retry,omitempty"`
	Failure *Failure `json:"failure,omitempty"`
	Runs    []*Run   `json:"runs,omitempty"`
}

type Run struct {
	Kind     string `json:"kind"`
	Workflow string `json:"workflow,omitempty"`
	File     string `json:"file,omitempty"`

	// CorrelationID identifies dispatched runs by their run name.
	CorrelationID string `json:"correlation_id,omitempty"`

	ID         int64  `json:"id,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	URL        string `json:"url,omitempty"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	Result     string `json:"result,omitempty"`
	// Details is extra text for the report, e.g. git am output.
	Details string `json:"details,omitempty"`

	Updated   string `json:"updated,omitempty"`
	Completed string `json:"completed,omitempty"`

	// MinAttempt makes a rerun wait for a newer attempt than this.
	MinAttempt int `json:"min_attempt,omitempty"`
	AutoReruns int `json:"auto_reruns,omitempty"`

	Pending Report   `json:"pending"`
	Final   Report   `json:"final"`
	Failure *Failure `json:"failure,omitempty"`
}

type Report struct {
	Check     string `json:"check,omitempty"`
	Email     string `json:"email,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

// Failure tracks a failing operation across runs.
type Failure struct {
	Op        string `json:"op"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
	First     string `json:"first"`
	Next      string `json:"next"`
}

const (
	minBackoff = 5 * time.Minute
	maxBackoff = 6 * time.Hour
)

func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func ParseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// RecordFailure updates f for a new failure of op and reports whether
// the deadline since the first failure has passed.
func RecordFailure(f **Failure, op string, err error, now time.Time, deadline time.Duration) bool {
	if *f == nil || (*f).Op != op {
		*f = &Failure{Op: op, First: FormatTime(now)}
	}
	cur := *f
	cur.Attempts++
	cur.LastError = trimError(err.Error())
	backoff := minBackoff << min(cur.Attempts-1, 10)
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	cur.Next = FormatTime(now.Add(backoff))
	return deadline > 0 && now.Sub(ParseTime(cur.First)) > deadline
}

func trimError(s string) string {
	const max = 500
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// Due reports whether an operation may be attempted now.
func (f *Failure) Due(now time.Time) bool {
	return f == nil || !now.Before(ParseTime(f.Next))
}

func (s *Series) Active() bool {
	switch s.Status {
	case SeriesDone, SeriesSkipped, SeriesError:
		return false
	}
	return true
}

func (s *Series) Patch(id int) *Patch {
	for _, p := range s.Patches {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (s *Series) Finish(status, reason string, now time.Time) {
	s.Status = status
	if reason != "" {
		s.Reason = reason
	}
	s.Finished = FormatTime(now)
}

func (p *Patch) Run(kind, workflow string) *Run {
	for _, r := range p.Runs {
		if r.Kind == kind && r.Workflow == workflow {
			return r
		}
	}
	return nil
}

func (p *Patch) RunByCorrelation(id string) *Run {
	for _, r := range p.Runs {
		if r.CorrelationID != "" && r.CorrelationID == id {
			return r
		}
	}
	return nil
}

// Settled reports whether a run has a final result whose report is
// fully handled.
func (r *Run) Settled() bool {
	return r.Result != "" && r.Status == "completed" &&
		(r.Final.Email == StepSent || r.Final.Email == StepSkipped || r.Final.Email == StepFailed)
}
