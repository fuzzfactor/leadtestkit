// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"leadtestkit/internal/logx"
)

type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML accepts Go durations plus a "d" (day) unit.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return v, nil
}

// Every option is documented in examples/reference.yaml; tests keep the
// two in sync.
type Config struct {
	Enabled bool   `yaml:"enabled"`
	Name    string `yaml:"name"`

	Patchwork Patchwork `yaml:"patchwork"`
	Upstream  Upstream  `yaml:"upstream"`
	TestRepo  TestRepo  `yaml:"test_repo"`
	Report    Report    `yaml:"report"`
	Archive   Archive   `yaml:"archive"`
	SMTP      SMTP      `yaml:"smtp"`
	Mode      Mode      `yaml:"mode"`
	Rules     Rules     `yaml:"rules"`
	Limits    Limits    `yaml:"limits"`
	Workflows Workflows `yaml:"workflows"`
	Rechecks  Rechecks  `yaml:"rechecks"`
	Runner    Runner    `yaml:"runner"`
	Pages     Pages     `yaml:"pages"`

	Secrets Secrets `yaml:"-"`
}

type Patchwork struct {
	APIURL string `yaml:"api_url"`
	// WebURL defaults to the API URL without /api/N.N.
	WebURL   string   `yaml:"web_url"`
	Project  string   `yaml:"project"`
	MinDelay Duration `yaml:"min_delay"`
	// CommentEvents selects comment discovery: events on API 1.3+,
	// otherwise the dev list archive.
	CommentEvents *bool `yaml:"comment_events"`
}

type Upstream struct {
	GitURL        string `yaml:"git_url"`
	DefaultBranch string `yaml:"default_branch"`
}

type TestRepo struct {
	// Repo is "owner/name" of the test fork on GitHub.
	Repo         string   `yaml:"repo"`
	ToolsBranch  string   `yaml:"tools_branch"`
	SyncInterval Duration `yaml:"sync_interval"`
	BranchPrefix string   `yaml:"branch_prefix"`
	StagingRef   string   `yaml:"staging_ref"`
	APIURL       string   `yaml:"api_url"`
	GitHost      string   `yaml:"git_host"`
}

type Report struct {
	// Style is "ovs" (patchwork state names) or "dpdk" (upper case).
	Style       string `yaml:"style"`
	To          string `yaml:"to"`
	From        string `yaml:"from"`
	Label       string `yaml:"label"`
	ApplyLabel  string `yaml:"apply_label"`
	BuildPrefix string `yaml:"build_prefix"`
	// CcSubmitter adds the series submitter on non-success reports.
	CcSubmitter  bool `yaml:"cc_submitter"`
	PendingEmail bool `yaml:"pending_email"`
	ApplySuccess bool `yaml:"apply_success"`
	PendingCheck bool `yaml:"pending_check"`
	LogLines     int  `yaml:"log_lines"`
	MaxLogJobs   int  `yaml:"max_log_jobs"`
	// MessageIDDomain is the right-hand side of generated Message-IDs.
	MessageIDDomain string `yaml:"message_id_domain"`
}

type Archive struct {
	ReportsURL string   `yaml:"reports_url"`
	DevURL     string   `yaml:"dev_url"`
	Ingest     bool     `yaml:"ingest"`
	MinDelay   Duration `yaml:"min_delay"`
	// LookbackMonths is how many months before the current one are
	// always scanned.
	LookbackMonths int `yaml:"lookback_months"`
	// MaxMessages bounds the messages read per run so a backlog cannot
	// consume the whole run.
	MaxMessages int `yaml:"max_messages"`
}

type SMTP struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// Security is "starttls" or "tls".
	Security string `yaml:"security"`
}

type Mode struct {
	Push              string   `yaml:"push"`
	Checks            string   `yaml:"checks"`
	Email             string   `yaml:"email"`
	AllowedRecipients []string `yaml:"allowed_recipients"`
}

func (m Mode) TestEmail() bool  { return m.Email == "test" }
func (m Mode) PostChecks() bool { return m.Checks == "post" }
func (m Mode) PushLive() bool   { return m.Push == "live" }
func (m Mode) EmailOff() bool   { return m.Email == "off" }

type Rules struct {
	Skip []SkipRule `yaml:"skip"`
	Base []BaseRule `yaml:"base"`
	// Supersede skips queued older versions of a series when a newer
	// version from the same submitter arrives.
	Supersede bool `yaml:"supersede"`
}

type SkipRule struct {
	Subject string `yaml:"subject"`
	Reason  string `yaml:"reason"`
	re      *regexp.Regexp
}

func (s *SkipRule) Regexp() *regexp.Regexp { return s.re }

type Tree struct {
	GitURL string `yaml:"git_url"`
	Branch string `yaml:"branch"`
}

type BaseRule struct {
	// Type is "subject", "delegate" or "maintainers".
	Type string `yaml:"type"`

	// subject: regexp on patch names; Branch may use $1 etc.
	Match  string `yaml:"match"`
	GitURL string `yaml:"git_url"`
	Branch string `yaml:"branch"`

	// delegate: patchwork username of the delegate.
	Delegate string `yaml:"delegate"`

	// maintainers: MAINTAINERS file in the base tree and a map from
	// tree names (e.g. dpdk-next-net) to git trees.
	File  string          `yaml:"file"`
	Trees map[string]Tree `yaml:"trees"`

	re *regexp.Regexp
}

func (b *BaseRule) Regexp() *regexp.Regexp { return b.re }

type Limits struct {
	MaxSeriesPatches   int      `yaml:"max_series_patches"`
	MaxQueuedJobs      int      `yaml:"max_queued_jobs"`
	MaxInflightPatches int      `yaml:"max_inflight_patches"`
	PerAuthorMin       int      `yaml:"per_author_min"`
	PerAuthorMaxShare  float64  `yaml:"per_author_max_share"`
	JobsPerPatch       int      `yaml:"jobs_per_patch"`
	SeriesMaxAge       Duration `yaml:"series_max_age"`
	InitialBackfill    Duration `yaml:"initial_backfill"`
	CompleteTimeout    Duration `yaml:"complete_timeout"`
	NoRunTimeout       Duration `yaml:"no_run_timeout"`
	RunTimeout         Duration `yaml:"run_timeout"`
	GraceAfterRuns     Duration `yaml:"grace_after_runs"`
	RetryDeadline      Duration `yaml:"retry_deadline"`
	BranchRetention    Duration `yaml:"branch_retention"`
	HistoryWindow      Duration `yaml:"history_window"`
}

type Workflows struct {
	// Expected lists workflow names that must report before a patch
	// is considered complete.  Without it, GraceAfterRuns is used.
	Expected []string `yaml:"expected"`
	// Custom enables dispatching workflows from the fork's tools
	// branch.
	Custom        bool     `yaml:"custom"`
	CustomPrefix  string   `yaml:"custom_prefix"`
	CustomExclude []string `yaml:"custom_exclude"`
	// CustomPerSeries lists custom workflow files that only run once
	// per series, on the last patch.
	CustomPerSeries []string `yaml:"custom_per_series"`
	RerunCancelled  bool     `yaml:"rerun_cancelled"`
	// RerunJobs is "failed" (GitHub's "Re-run failed jobs") or "all".
	RerunJobs string `yaml:"rerun_jobs"`
}

type Rechecks struct {
	Enabled bool `yaml:"enabled"`
	// Labels accepted in "Recheck-request:" lines.  Defaults to the
	// report label.
	Labels []string `yaml:"labels"`
	Window Duration `yaml:"window"`
}

type Runner struct {
	LoopDuration Duration `yaml:"loop_duration"`
	HardStop     Duration `yaml:"hard_stop"`
	LoopInterval Duration `yaml:"loop_interval"`
	CommitAuthor string   `yaml:"commit_author"`
}

type Pages struct {
	Enabled bool   `yaml:"enabled"`
	Title   string `yaml:"title"`
	// ArchiveURLFormat and PatchArchiveURLFormat link report and patch
	// emails by Message-ID, e.g. https://inbox.dpdk.org/dev/{}/ for lore.
	ArchiveURLFormat      string `yaml:"archive_url_format"`
	PatchArchiveURLFormat string `yaml:"patch_archive_url_format"`
}

type Secrets struct {
	PatchworkToken string
	// AppClientID and AppPrivateKey identify the GitHub App installed
	// on the test fork.  The App ID works in place of the Client ID.
	AppClientID   string
	AppPrivateKey string
	SMTPUsername  string
	SMTPPassword  string
}

func defaults() Config {
	return Config{
		Patchwork: Patchwork{MinDelay: Duration(5 * time.Second)},
		Upstream:  Upstream{DefaultBranch: "main"},
		TestRepo: TestRepo{
			SyncInterval: Duration(7 * 24 * time.Hour),
			BranchPrefix: "series_",
			StagingRef:   "refs/leadtest/",
			APIURL:       "https://api.github.com",
			GitHost:      "github.com",
		},
		Report: Report{
			Style:        "ovs",
			Label:        "github-robot",
			ApplyLabel:   "apply-robot",
			BuildPrefix:  "github build",
			CcSubmitter:  true,
			PendingEmail: true,
			ApplySuccess: true,
			PendingCheck: true,
			LogLines:     25,
			MaxLogJobs:   5,
		},
		Archive: Archive{
			MinDelay:       Duration(time.Second),
			LookbackMonths: 1,
			MaxMessages:    200,
		},
		SMTP: SMTP{Port: 587, Security: "starttls"},
		Mode: Mode{Push: "live", Checks: "log", Email: "test"},
		Limits: Limits{
			MaxSeriesPatches:   50,
			MaxQueuedJobs:      20,
			MaxInflightPatches: 20,
			PerAuthorMin:       2,
			PerAuthorMaxShare:  0.5,
			JobsPerPatch:       10,
			SeriesMaxAge:       Duration(7 * 24 * time.Hour),
			CompleteTimeout:    Duration(3 * 24 * time.Hour),
			NoRunTimeout:       Duration(2 * time.Hour),
			RunTimeout:         Duration(30 * time.Hour),
			GraceAfterRuns:     Duration(20 * time.Minute),
			RetryDeadline:      Duration(2 * 24 * time.Hour),
			BranchRetention:    Duration(730 * 24 * time.Hour),
			HistoryWindow:      0,
		},
		Workflows: Workflows{RerunCancelled: true, RerunJobs: "failed",
			CustomPrefix: "leadtest-"},
		Rechecks: Rechecks{Enabled: true, Window: Duration(30 * 24 * time.Hour)},
		Runner: Runner{
			LoopDuration: Duration(13 * time.Minute),
			HardStop:     Duration(40 * time.Minute),
			LoopInterval: Duration(3 * time.Minute),
			CommitAuthor: "github-actions[bot] " +
				"<41898282+github-actions[bot]@users.noreply.github.com>",
		},
		Pages: Pages{Enabled: true, Title: "LeadTestKit"},
	}
}

func Parse(data []byte) (*Config, error) {
	cfg := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var ErrNotConfigured = errors.New("not configured")

// Load reads config.yaml.  A missing file is ErrNotConfigured so that
// unconfigured forks do nothing.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s does not exist", ErrNotConfigured, path)
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// LoadSecrets reads secrets from the environment and registers them
// for redaction.
func (c *Config) LoadSecrets(getenv func(string) string) {
	c.Secrets = Secrets{
		PatchworkToken: strings.TrimSpace(getenv("LEADTEST_PW_TOKEN")),
		AppClientID:    strings.TrimSpace(getenv("LEADTEST_APP_CLIENT_ID")),
		AppPrivateKey:  strings.TrimSpace(getenv("LEADTEST_APP_PRIVATE_KEY")),
		SMTPUsername:   strings.TrimSpace(getenv("LEADTEST_SMTP_USERNAME")),
		SMTPPassword:   getenv("LEADTEST_SMTP_PASSWORD"),
	}
	logx.AddSecret(c.Secrets.PatchworkToken)
	// Line by line: a key would show up in logs split into lines.
	for _, line := range strings.Split(c.Secrets.AppPrivateKey, "\n") {
		if !strings.HasPrefix(line, "-----") {
			logx.AddSecret(strings.TrimSpace(line))
		}
	}
	logx.AddSecret(c.Secrets.SMTPPassword)
}

func (c *Config) CheckSecrets() error {
	var missing []string
	if c.Mode.PostChecks() && c.Secrets.PatchworkToken == "" {
		missing = append(missing, "LEADTEST_PW_TOKEN")
	}
	app := c.Secrets.AppClientID != "" || c.Secrets.AppPrivateKey != ""
	if c.Mode.PushLive() || app {
		if c.Secrets.AppClientID == "" {
			missing = append(missing, "LEADTEST_APP_CLIENT_ID")
		}
		if c.Secrets.AppPrivateKey == "" {
			missing = append(missing, "LEADTEST_APP_PRIVATE_KEY")
		}
	}
	if !c.Mode.EmailOff() && c.Secrets.SMTPUsername != "" && c.Secrets.SMTPPassword == "" {
		missing = append(missing, "LEADTEST_SMTP_PASSWORD")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing secrets: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (c *Config) PatchworkWebURL() string {
	if c.Patchwork.WebURL != "" {
		return strings.TrimRight(c.Patchwork.WebURL, "/")
	}
	return webFromAPI(c.Patchwork.APIURL)
}

var commitAuthorRe = regexp.MustCompile(`^[^<>]+ <[^<>@\s]+@[^<>\s]+>$`)

var apiSuffixRe = regexp.MustCompile(`/api(/\d+\.\d+)?/?$`)

func webFromAPI(api string) string {
	return strings.TrimRight(apiSuffixRe.ReplaceAllString(strings.TrimRight(api, "/"), ""), "/")
}

func (c *Config) apiVersion() string {
	m := apiSuffixRe.FindStringSubmatch(strings.TrimRight(c.Patchwork.APIURL, "/"))
	if len(m) < 2 {
		return ""
	}
	return strings.TrimPrefix(m[1], "/")
}

// UseCommentEvents reports whether comment events are available.
// Patchwork API 1.2 does not have them.
func (c *Config) UseCommentEvents() bool {
	if c.Patchwork.CommentEvents != nil {
		return *c.Patchwork.CommentEvents
	}
	v := c.apiVersion()
	return v != "" && v >= "1.3"
}

func (c *Config) RecheckLabels() []string {
	if len(c.Rechecks.Labels) > 0 {
		return c.Rechecks.Labels
	}
	return []string{c.Report.Label}
}

func (c *Config) validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	checkURL := func(field, v string, required bool) {
		if v == "" {
			if required {
				add("%s is required", field)
			}
			return
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			add("%s: invalid URL %q", field, v)
		}
	}
	checkURL("patchwork.api_url", c.Patchwork.APIURL, true)
	checkURL("patchwork.web_url", c.Patchwork.WebURL, false)
	if c.Patchwork.Project == "" {
		add("patchwork.project is required")
	}
	if v := c.apiVersion(); v != "" && v < "1.2" {
		add("patchwork API version %s is not supported (need 1.2+)", v)
	}
	if c.Upstream.GitURL == "" {
		add("upstream.git_url is required")
	}
	if c.Upstream.DefaultBranch == "" {
		add("upstream.default_branch is required")
	}
	if c.TestRepo.ToolsBranch == "" {
		c.TestRepo.ToolsBranch = c.Upstream.DefaultBranch
	}
	if parts := strings.Split(c.TestRepo.Repo, "/"); len(parts) != 2 ||
		parts[0] == "" || parts[1] == "" {
		add("test_repo.repo must be owner/name, got %q", c.TestRepo.Repo)
	}
	if !strings.HasPrefix(c.TestRepo.StagingRef, "refs/") ||
		!strings.HasSuffix(c.TestRepo.StagingRef, "/") ||
		strings.HasPrefix(c.TestRepo.StagingRef, "refs/heads/") ||
		strings.HasPrefix(c.TestRepo.StagingRef, "refs/tags/") {
		add("test_repo.staging_ref must be a refs/<namespace>/ outside heads and tags")
	}
	checkURL("test_repo.api_url", c.TestRepo.APIURL, true)

	switch c.Report.Style {
	case "ovs", "dpdk":
	default:
		add("report.style must be ovs or dpdk")
	}
	if c.Report.Label == "" || c.Report.ApplyLabel == "" {
		add("report.label and report.apply_label must not be empty")
	}

	switch c.Mode.Push {
	case "live", "off":
	default:
		add("mode.push must be live or off")
	}
	switch c.Mode.Checks {
	case "post", "log":
	default:
		add("mode.checks must be post or log")
	}
	switch c.Mode.Email {
	case "live", "test", "off":
	default:
		add("mode.email must be live, test or off")
	}
	if !c.Mode.EmailOff() {
		if _, err := mail.ParseAddress(c.Report.To); err != nil {
			add("report.to: %v", err)
		}
		if _, err := mail.ParseAddress(c.Report.From); err != nil {
			add("report.from: %v", err)
		}
		if c.SMTP.Host == "" {
			add("smtp.host is required unless mode.email is off")
		}
		switch c.SMTP.Security {
		case "starttls", "tls":
		default:
			add("smtp.security must be starttls or tls")
		}
		// Test mode must never reach anyone who did not opt in.
		if c.Mode.TestEmail() && len(c.Mode.AllowedRecipients) == 0 {
			add("mode.allowed_recipients is required when mode.email is test")
		}
	}
	for _, a := range c.Mode.AllowedRecipients {
		if _, err := mail.ParseAddress(a); err != nil {
			add("mode.allowed_recipients: %v", err)
		}
	}
	if c.Report.MessageIDDomain == "" {
		if a, err := mail.ParseAddress(c.Report.From); err == nil {
			if i := strings.LastIndex(a.Address, "@"); i >= 0 {
				c.Report.MessageIDDomain = a.Address[i+1:]
			}
		}
		if c.Report.MessageIDDomain == "" {
			c.Report.MessageIDDomain = "leadtestkit.invalid"
		}
	}

	checkURL("archive.reports_url", c.Archive.ReportsURL, c.Archive.Ingest)
	checkURL("archive.dev_url", c.Archive.DevURL, false)
	if c.Rechecks.Enabled && !c.UseCommentEvents() && c.Archive.DevURL == "" {
		add("archive.dev_url is required for rechecks without comment events")
	}

	for i := range c.Rules.Skip {
		r := &c.Rules.Skip[i]
		re, err := regexp.Compile(r.Subject)
		if err != nil {
			add("rules.skip[%d]: %v", i, err)
			continue
		}
		r.re = re
	}
	for i := range c.Rules.Base {
		r := &c.Rules.Base[i]
		switch r.Type {
		case "subject":
			re, err := regexp.Compile(r.Match)
			if err != nil || r.Match == "" {
				add("rules.base[%d]: invalid match %q", i, r.Match)
				continue
			}
			r.re = re
			if r.Branch == "" {
				add("rules.base[%d]: branch is required", i)
			}
		case "delegate":
			if r.Delegate == "" || r.Branch == "" {
				add("rules.base[%d]: delegate and branch are required", i)
			}
		case "maintainers":
			if r.File == "" {
				r.File = "MAINTAINERS"
			}
			for name, t := range r.Trees {
				if t.GitURL == "" || t.Branch == "" {
					add("rules.base[%d]: tree %s needs git_url and branch",
						i, name)
				}
			}
		default:
			add("rules.base[%d]: unknown type %q", i, r.Type)
		}
	}

	l := c.Limits
	if l.HistoryWindow < 0 {
		add("limits.history_window must be >= 0 (0 searches all history)")
	}
	if l.MaxQueuedJobs < 1 || l.MaxInflightPatches < 1 || l.PerAuthorMin < 1 ||
		l.JobsPerPatch < 1 {
		add("limits: max_queued_jobs, max_inflight_patches, per_author_min " +
			"and jobs_per_patch must be >= 1")
	}
	if l.PerAuthorMaxShare <= 0 || l.PerAuthorMaxShare > 1 {
		add("limits.per_author_max_share must be in (0, 1]")
	}
	if c.Runner.LoopInterval <= 0 || c.Runner.LoopDuration < 0 {
		add("runner: loop_interval must be positive")
	}
	if c.Runner.HardStop < c.Runner.LoopDuration {
		add("runner.hard_stop must not be shorter than runner.loop_duration")
	}
	switch c.Workflows.RerunJobs {
	case "failed", "all":
	default:
		add("workflows.rerun_jobs must be failed or all")
	}
	if !commitAuthorRe.MatchString(c.Runner.CommitAuthor) {
		add("runner.commit_author must be \"Name <email>\"")
	}

	if len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}
