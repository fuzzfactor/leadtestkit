// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
enabled: true
patchwork:
  api_url: https://pw.example.org/api/1.2
  project: lorem
upstream:
  git_url: https://git.example.org/lorem.git
test_repo:
  repo: ipsum/lorem
report:
  to: lorem-build@lists.example.org
  from: "Robot <robot@example.org>"
smtp:
  host: smtp.example.org
mode:
  allowed_recipients: [lorem-build@lists.example.org]
archive:
  dev_url: https://lists.example.org/pipermail/lorem-dev/
`

func TestParseMinimalDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode.Push != "live" || c.Mode.Checks != "log" || c.Mode.Email != "test" {
		t.Errorf("mode = %+v", c.Mode)
	}
	if c.Report.Label != "github-robot" || c.Report.Style != "ovs" {
		t.Errorf("report = %+v", c.Report)
	}
	if c.PatchworkWebURL() != "https://pw.example.org" {
		t.Errorf("web url = %q", c.PatchworkWebURL())
	}
	if c.UseCommentEvents() {
		t.Error("API 1.2 has no comment events")
	}
	if c.Report.MessageIDDomain != "example.org" {
		t.Errorf("message id domain = %q", c.Report.MessageIDDomain)
	}
	if c.Limits.SeriesMaxAge.D() != 7*24*time.Hour {
		t.Errorf("series max age = %v", c.Limits.SeriesMaxAge.D())
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Parse([]byte(minimal + "\nlimits:\n  max_queud_jobs: 3\n"))
	if err == nil || !strings.Contains(err.Error(), "max_queud_jobs") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := map[string]string{
		"test mode without allowlist": strings.Replace(minimal,
			"  allowed_recipients: [lorem-build@lists.example.org]\n", "", 1),
		"bad repo":    strings.Replace(minimal, "repo: ipsum/lorem", "repo: lorem", 1),
		"old api":     strings.Replace(minimal, "api/1.2", "api/1.1", 1),
		"bad mode":    minimal + "\n  push: maybe\n",
		"bad regexp":  minimal + "\nrules:\n  skip:\n    - subject: '('\n",
		"bad base":    minimal + "\nrules:\n  base:\n    - type: lorem\n",
		"staging ref": minimal + "\n  staging_ref: refs/heads/x/\n",
		"no dev archive for rechecks": strings.Replace(minimal,
			"archive:\n  dev_url: https://lists.example.org/pipermail/lorem-dev/\n", "", 1),
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if name == "bad mode" {
				cfg = strings.Replace(minimal, "mode:\n", "mode:\n  push: maybe\n", 1)
			}
			if name == "staging ref" {
				cfg = strings.Replace(minimal, "test_repo:\n", "test_repo:\n  staging_ref: refs/heads/x/\n", 1)
			}
			if _, err := Parse([]byte(cfg)); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestDuration(t *testing.T) {
	c, err := Parse([]byte(minimal + "\nlimits:\n  series_max_age: 3d\n  grace_after_runs: 90m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.SeriesMaxAge.D() != 72*time.Hour || c.Limits.GraceAfterRuns.D() != 90*time.Minute {
		t.Errorf("limits = %+v", c.Limits)
	}
	if _, err := Parse([]byte(minimal + "\nlimits:\n  series_max_age: lorem\n")); err == nil {
		t.Error("expected error for invalid duration")
	}
}

func TestBaseRuleRegexp(t *testing.T) {
	c, err := Parse([]byte(minimal + `
rules:
  base:
    - type: subject
      match: '\b(branch-\d+\.\d+)\b'
      branch: '$1'
`))
	if err != nil {
		t.Fatal(err)
	}
	re := c.Rules.Base[0].Regexp()
	if re == nil || re.ReplaceAllString(re.FindString("[PATCH branch-3.4] x"), c.Rules.Base[0].Branch) !=
		"branch-3.4" {
		t.Errorf("regexp = %v", re)
	}
}

func TestLoadMissingIsNotConfigured(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "config.yaml"))
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v", err)
	}
}

func TestSecrets(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"LEADTEST_GITHUB_TOKEN": " lorem-gh-token \n"}
	c.LoadSecrets(func(k string) string { return env[k] })
	if c.Secrets.GitHubToken != "lorem-gh-token" {
		t.Errorf("token = %q", c.Secrets.GitHubToken)
	}
	if err := c.CheckSecrets(); err != nil {
		t.Errorf("log mode needs no patchwork token: %v", err)
	}
	c.Secrets.GitHubToken = ""
	if err := c.CheckSecrets(); err == nil {
		t.Error("pushing needs a GitHub token")
	}
	c.Mode.Push = "off"
	if err := c.CheckSecrets(); err != nil {
		t.Errorf("no push needs no GitHub token: %v", err)
	}
	c.Secrets.GitHubToken = "lorem-gh-token"
	c.Mode.Checks = "post"
	if err := c.CheckSecrets(); err == nil || !strings.Contains(err.Error(), "LEADTEST_PW_TOKEN") {
		t.Errorf("err = %v", err)
	}
}

func TestExamplesParse(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no examples: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(data); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
