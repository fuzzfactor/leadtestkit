// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"leadtestkit/internal/logx"
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
	if c.Limits.SeriesMaxAge.D() != 30*24*time.Hour {
		t.Errorf("series max age = %v", c.Limits.SeriesMaxAge.D())
	}
	if c.Limits.HistoryWindow != 0 {
		t.Errorf("history should be unlimited by default: %v", c.Limits.HistoryWindow.D())
	}
	if c.Limits.BranchRetention.D() != 730*24*time.Hour {
		t.Errorf("branch retention = %v, want 730d", c.Limits.BranchRetention.D())
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
		"bad files":   minimal + "\nrules:\n  skip:\n    - files: '('\n",
		"empty skip":  minimal + "\nrules:\n  skip:\n    - reason: lorem\n",
		"bad base":    minimal + "\nrules:\n  base:\n    - type: lorem\n",
		"staging ref": minimal + "\n  staging_ref: refs/heads/x/\n",
		"history":     minimal + "\nlimits:\n  history_window: -1d\n",
		"bad alert":   minimal + "\nalerts:\n  to: [lorem]\n",
		"stale":       minimal + "\npages:\n  stale_after: -1h\n",
		"stale 1s":    minimal + "\npages:\n  stale_after: 1s\n",
		"no dev archive for rechecks": strings.Replace(minimal,
			"archive:\n  dev_url: https://lists.example.org/pipermail/lorem-dev/\n",
			"", 1),
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if name == "bad mode" {
				cfg = strings.Replace(minimal, "mode:\n",
					"mode:\n  push: maybe\n", 1)
			}
			if name == "staging ref" {
				cfg = strings.Replace(minimal, "test_repo:\n",
					"test_repo:\n  staging_ref: refs/heads/x/\n", 1)
			}
			if _, err := Parse([]byte(cfg)); err == nil {
				t.Error("expected validation error")
			}
		})
	}
}

func TestDuration(t *testing.T) {
	c, err := Parse([]byte(minimal +
		"\nlimits:\n  series_max_age: 3d\n  grace_after_runs: 90m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits.SeriesMaxAge.D() != 72*time.Hour ||
		c.Limits.GraceAfterRuns.D() != 90*time.Minute {
		t.Errorf("limits = %+v", c.Limits)
	}
	if _, err := Parse([]byte(minimal + "\nlimits:\n  series_max_age: lorem\n")); err == nil {
		t.Error("expected error for invalid duration")
	}
}

func TestHistoryWindow(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"0", 0},
		{"30d", 30 * 24 * time.Hour},
	} {
		c, err := Parse([]byte(minimal + "\nlimits:\n  history_window: " + tc.value + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if c.Limits.HistoryWindow.D() != tc.want {
			t.Errorf("history_window %s = %v", tc.value, c.Limits.HistoryWindow.D())
		}
		if c.Rechecks.Window.D() != 30*24*time.Hour {
			t.Error("history_window changed the independent recheck window")
		}
	}
}

func TestBranchRetentionOverrides(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"0", 0},
		{"30d", 30 * 24 * time.Hour},
		{"365d", 365 * 24 * time.Hour},
	} {
		data := minimal + "\nlimits:\n  branch_retention: " + tc.value + "\n"
		c, err := Parse([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if c.Limits.BranchRetention.D() != tc.want {
			t.Errorf("branch_retention %s = %v", tc.value, c.Limits.BranchRetention.D())
		}
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
	if re == nil || re.ReplaceAllString(re.FindString("[PATCH branch-3.4] x"),
		c.Rules.Base[0].Branch) !=
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
	// Encoded here, so that no key-like text is in the source.
	key := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
		Bytes: []byte("loremipsumdolorsitamet")}))
	env := map[string]string{"LEADTEST_APP_CLIENT_ID": " Iv23lilorem \n",
		"LEADTEST_APP_PRIVATE_KEY": key}
	c.LoadSecrets(func(k string) string { return env[k] })
	if c.Secrets.AppClientID != "Iv23lilorem" ||
		c.Secrets.AppPrivateKey != strings.TrimSpace(key) {
		t.Errorf("secrets = %+v", c.Secrets)
	}
	if body := strings.Split(key, "\n")[1]; logx.Redact("key "+body) != "key [REDACTED]" {
		t.Error("the private key is not redacted")
	}
	if err := c.CheckSecrets(); err != nil {
		t.Errorf("log mode needs no patchwork token: %v", err)
	}
	c.Secrets.AppPrivateKey = ""
	if err := c.CheckSecrets(); err == nil ||
		!strings.Contains(err.Error(), "LEADTEST_APP_PRIVATE_KEY") {
		t.Errorf("a Client ID without a key must be refused: %v", err)
	}
	c.Mode.Push = "off"
	c.Secrets.AppClientID = ""
	if err := c.CheckSecrets(); err != nil {
		t.Errorf("no push needs no GitHub App: %v", err)
	}
	c.Mode.Push = "live"
	if err := c.CheckSecrets(); err == nil ||
		!strings.Contains(err.Error(), "LEADTEST_APP_CLIENT_ID, LEADTEST_APP_PRIVATE_KEY") {
		t.Errorf("pushing needs the GitHub App: %v", err)
	}
	c.Secrets.AppClientID, c.Secrets.AppPrivateKey = "Iv23lilorem", key
	c.Mode.Checks = "post"
	if err := c.CheckSecrets(); err == nil ||
		!strings.Contains(err.Error(), "LEADTEST_PW_TOKEN") {
		t.Errorf("err = %v", err)
	}
}

func TestOVSExampleRules(t *testing.T) {
	c, err := Load("../../examples/ovs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	skip := func(name string) string {
		for _, r := range c.Rules.Skip {
			if r.Regexp() != nil && r.Regexp().MatchString(name) {
				return r.Reason
			}
		}
		return ""
	}
	skipFile := func(file string) string {
		for _, r := range c.Rules.Skip {
			if r.FilesRegexp() != nil && r.FilesRegexp().MatchString(file) {
				return r.Reason
			}
		}
		return ""
	}
	base := func(name string) string {
		for _, r := range c.Rules.Base {
			re := r.Regexp()
			if m := re.FindStringSubmatchIndex(name); m != nil {
				return string(re.ExpandString(nil, r.Branch, name, m))
			}
		}
		return ""
	}
	for name, want := range map[string]string{
		"[ovs-dev,RFC,v2,1/3] lorem: Ipsum.":          "RFC",
		"[ovs-dev,RFC] lorem: Ipsum.":                 "RFC",
		"[ovs-dev,net-next,v2] lorem: Ipsum.":         "kernel patch",
		"[ovs-dev] lorem: Implement RFC 8200 bits.":   "",
		"[ovs-dev,v2,1/3] lorem: Drop net-next hack.": "",
	} {
		if got := skip(name); got != want {
			t.Errorf("skip %q = %q, want %q", name, got, want)
		}
	}
	for file, want := range map[string]string{
		"net/openvswitch/flow.c":                         "kernel patch",
		"include/uapi/linux/openvswitch.h":               "kernel patch",
		"tools/testing/selftests/net/openvswitch/ovs.sh": "kernel patch",
		"lib/netdev-linux.c":                             "",
		"include/linux/openvswitch.h":                    "",
		"tests/system-traffic.at":                        "",
	} {
		if got := skipFile(file); got != want {
			t.Errorf("skip file %q = %q, want %q", file, got, want)
		}
	}
	for name, want := range map[string]string{
		"[ovs-dev,branch-3.4,v2,1/2] lorem: Ipsum.":     "branch-3.4",
		"[ovs-dev,dpdk-latest] lorem: Ipsum.":           "dpdk-latest",
		"[ovs-dev,v2] lorem: Backport from branch-3.4.": "",
		"[ovs-dev] lorem: Test with dpdk-latest.":       "",
	} {
		if got := base(name); got != want {
			t.Errorf("base %q = %q, want %q", name, got, want)
		}
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

// yamlKeys lists the keys of all options, following nested structs,
// slices of structs and maps of structs.
func yamlKeys(t reflect.Type, out map[string]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == reflect.TypeOf(Duration(0)) {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
		yamlKeys(f.Type, out)
	}
}

func TestReferenceDocumentsEveryOption(t *testing.T) {
	data, err := os.ReadFile("../../examples/reference.yaml")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	yamlKeys(reflect.TypeOf(Config{}), keys)
	for k := range keys {
		// Options of list items may only appear in commented examples.
		re := regexp.MustCompile(`(?m)^[ \t]*(#[ \t]*)?(- )?` + regexp.QuoteMeta(k) + `:`)
		if !re.Match(data) {
			t.Errorf("option %q is not documented in examples/reference.yaml", k)
		}
	}
}

func TestReferenceShowsDefaults(t *testing.T) {
	data, err := os.ReadFile("../../examples/reference.yaml")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Parse([]byte(`
patchwork:
  api_url: https://patchwork.example.org/api/1.2
  project: lorem
upstream:
  git_url: https://github.com/example/lorem.git
test_repo:
  repo: example/lorem
report:
  to: lorem-build@lists.example.org
  from: "LeadTestKit Robot <robot@example.org>"
archive:
  dev_url: https://lists.example.org/pipermail/lorem-dev/
smtp:
  host: smtp.example.org
mode:
  allowed_recipients: [lorem-build@lists.example.org]
`))
	if err != nil {
		t.Fatal(err)
	}
	// An empty list in the reference is the same as no list.
	norm := func(c *Config) {
		for _, l := range []*[]string{&c.Workflows.Expected, &c.Workflows.CustomExclude,
			&c.Workflows.CustomPerSeries, &c.Rechecks.Labels, &c.Alerts.To} {
			if len(*l) == 0 {
				*l = nil
			}
		}
		if len(c.Rules.Skip) == 0 {
			c.Rules.Skip = nil
		}
		if len(c.Rules.Base) == 0 {
			c.Rules.Base = nil
		}
	}
	norm(got)
	norm(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reference values differ from the defaults:\n got: %+v\nwant: %+v",
			got, want)
	}
}
