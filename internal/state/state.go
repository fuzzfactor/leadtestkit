// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"time"
)

const (
	SeriesNew     = "new"
	SeriesQueued  = "queued"
	SeriesWaiting = "waiting"
	SeriesTesting = "testing"
	SeriesDone    = "done"
	SeriesSkipped = "skipped"
	SeriesError   = "error"
)

const (
	PatchPending     = "pending"
	PatchApplied     = "applied"
	PatchApplyFailed = "apply-failed"
	PatchNotApplied  = "not-applied"
	PatchPushed      = "pushed"
	PatchDone        = "done"
	PatchSkipped     = "skipped"
)

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

const (
	StepPosted  = "posted"
	StepLogged  = "logged"
	StepSending = "sending"
	StepSent    = "sent"
	StepSkipped = "skipped"
	StepFailed  = "failed"
)

type Meta struct {
	Schema int `json:"schema"`
	// DiscoveryStart is when the bot first looked for series; older
	// series are only tested within limits.initial_backfill of it.
	DiscoveryStart string    `json:"discovery_start,omitempty"`
	Instance       *Instance `json:"instance,omitempty"`
	// ToolsSynced is when the tools branch was last synced with
	// upstream.
	ToolsSynced string                    `json:"tools_synced,omitempty"`
	Events      map[string]int            `json:"events"`
	Archives    map[string]*ArchiveCursor `json:"archives"`
	Stats       Stats                     `json:"stats"`
	// Outages holds the services that are unavailable, by kind of
	// outage, like OutageRuns, while it lasts.
	Outages map[string]*Outage `json:"outages,omitempty"`
}

// Outage records that a service the bot needs is unavailable, so that
// patches wait instead of failing.
type Outage struct {
	Since  string `json:"since"`
	Reason string `json:"reason"`
	// NextProbe is when one patch is pushed to check whether workflows
	// run again.
	NextProbe string `json:"next_probe,omitempty"`
	// Alerted is when the maintainers were last alerted.
	Alerted string `json:"alerted,omitempty"`
	// AlertFailing is since when alerts cannot be sent because of the
	// SMTP server.
	AlertFailing string `json:"alert_failing,omitempty"`
	// Checks is set on an outage of Patchwork that only refuses checks;
	// it ends when a check is accepted.
	Checks bool `json:"checks,omitempty"`
}

const (
	// OutageRuns: pushes start no workflow runs, for example because
	// GitHub disabled Actions on the fork.  It ends with any new run.
	OutageRuns = "runs"
	// OutageDispatch: GitHub refuses workflow dispatches.  It ends
	// when GitHub accepts one.
	OutageDispatch = "dispatch"
	// OutageGitHub: GitHub refuses the GitHub App of the bot.
	OutageGitHub = "github"
	// OutageEmail: the SMTP server cannot be used.
	OutageEmail = "email"
	// OutagePatchwork: Patchwork does not answer, refuses the token or
	// refuses checks.
	OutagePatchwork = "patchwork"
)

// OutageArchive is the kind of outage of the archive with this cursor.
func OutageArchive(cursor string) string {
	return cursor + "-archive"
}

type ArchiveCursor struct {
	// LastMonth is the latest month (YYYY-MM) that was scanned.
	LastMonth string `json:"last_month,omitempty"`
	// Months maps pipermail month names to the highest message number
	// that was handled.
	Months map[string]int `json:"months"`
	// Retry holds messages that failed and will be read again.
	Retry map[string]*Failure `json:"retry,omitempty"`
	// Failing is since when the index of the archive cannot be read.
	Failing string `json:"failing,omitempty"`
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

type Decision struct {
	Time   string `json:"time"`
	Action string `json:"action"`
	Detail string `json:"detail"`
	RunURL string `json:"run_url,omitempty"`
	// WorkflowURL links the attempt of the test workflow run the
	// decision is about.
	WorkflowURL string `json:"workflow_url,omitempty"`
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

	// Reapply makes a series be applied again on the current base
	// instead of reusing commits of an earlier attempt.
	Reapply bool `json:"reapply,omitempty"`

	DependsOn int    `json:"depends_on,omitempty"`
	Base      *Base  `json:"base,omitempty"`
	Branch    string `json:"branch,omitempty"`

	Discovered string `json:"discovered"`
	// Checked is when an incomplete series was last looked at.
	Checked  string `json:"checked,omitempty"`
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished,omitempty"`

	Failure   *Failure   `json:"failure,omitempty"`
	Patches   []*Patch   `json:"patches"`
	Decisions []Decision `json:"decisions,omitempty"`
}

type Patch struct {
	ID     int    `json:"id"`
	Number int    `json:"number"`
	Name   string `json:"name"`
	MsgID  string `json:"msgid"`
	// Subject and Sender are of the patch email, for replies to it.
	// Sender has no email when a mailing list rewrote From and kept no
	// original address.
	Subject string  `json:"subject,omitempty"`
	Sender  *Person `json:"sender,omitempty"`
	Status  string  `json:"status"`
	SHA     string  `json:"sha,omitempty"`
	Pushed  string  `json:"pushed,omitempty"`

	// CommentsSeen is the highest comment ID checked for rechecks;
	// CommentsPending asks for the comments to be fetched.
	CommentsSeen    int  `json:"comments_seen,omitempty"`
	CommentsPending bool `json:"comments_pending,omitempty"`

	// Recheck requests a rerun of the failed runs and RecheckChecks of
	// the runs with these Patchwork check names, from a Recheck-request
	// comment or a human.
	Recheck       bool       `json:"recheck,omitempty"`
	RecheckChecks []string   `json:"recheck_checks,omitempty"`
	Retry         bool       `json:"retry,omitempty"`
	Failure       *Failure   `json:"failure,omitempty"`
	Runs          []*Run     `json:"runs,omitempty"`
	Decisions     []Decision `json:"decisions,omitempty"`
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

	Pending Report `json:"pending"`
	Final   Report `json:"final"`
	// Reply is the reply to the patch email about a failure.
	Reply   Report   `json:"reply"`
	Failure *Failure `json:"failure,omitempty"`
	// CheckFailure delays retries of the pending check.  It is kept
	// apart from Failure so that the emails do not wait for it.
	CheckFailure *Failure `json:"check_failure,omitempty"`
	// ReplyFailure delays retries of the reply.
	ReplyFailure *Failure `json:"reply_failure,omitempty"`
}

type Report struct {
	Check     string `json:"check,omitempty"`
	Email     string `json:"email,omitempty"`
	MessageID string `json:"message_id,omitempty"`
}

type Failure struct {
	Op        string `json:"op"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
	First     string `json:"first"`
	Next      string `json:"next"`
	// Outage is the kind of outage the failure waits for, without a
	// deadline.  The waiting does not count once it ends.
	Outage string `json:"outage,omitempty"`
	// Cause is the kind of outage the last error points at, even before
	// that outage started; the failure waits for it once it does.
	Cause string `json:"cause,omitempty"`
}

const (
	minBackoff = 5 * time.Minute
	maxBackoff = 3 * 24 * time.Hour
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

// Finish ends the series.  Unless it is done, patches that did not get
// a final status are marked skipped, so that they do not look pending.
func (s *Series) Finish(status, reason string, now time.Time) {
	s.Status = status
	if reason != "" {
		s.Reason = reason
	}
	s.Finished = FormatTime(now)
	if status == SeriesDone {
		return
	}
	for _, p := range s.Patches {
		if p.Unfinished() {
			p.Status = PatchSkipped
		}
	}
}

// Unfinished reports whether the patch is still on its way to a final
// status.
func (p *Patch) Unfinished() bool {
	switch p.Status {
	case PatchPending, PatchApplied, PatchPushed:
		return true
	}
	return false
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

// Settled reports whether a run has a final result whose report and
// reply are fully handled.
func (r *Run) Settled() bool {
	return r.Result != "" && r.Status == "completed" && handled(r.Final.Email) &&
		handled(r.Reply.Email)
}

func handled(step string) bool {
	return step == StepSent || step == StepSkipped || step == StepFailed
}
