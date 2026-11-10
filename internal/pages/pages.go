// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

// Package pages renders the GitHub Pages site.  The site is static and
// reads the same JSON files as the bot, plus a small index.
package pages

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"leadtestkit/internal/config"
	"leadtestkit/internal/pw"
	"leadtestkit/internal/state"
)

//go:embed site
var site embed.FS

type Month struct {
	Month   string `json:"month"`
	Series  int    `json:"series"`
	Patches int    `json:"patches"`
}

type Index struct {
	Schema           int    `json:"schema"`
	Title            string `json:"title"`
	Name             string `json:"name"`
	Generated        string `json:"generated"`
	RunURL           string `json:"run_url,omitempty"`
	RunFailed        bool   `json:"run_failed,omitempty"`
	PatchworkWeb     string `json:"patchwork_web"`
	Project          string `json:"project"`
	TestRepo         string `json:"test_repo"`
	GitHost          string `json:"git_host"`
	ArchiveURLFormat string `json:"archive_url_format,omitempty"`
	PatchURLFormat   string `json:"patch_archive_url_format,omitempty"`
	ListTag          string `json:"list_tag,omitempty"`
	// Outages are the services that are unavailable, by kind.
	Outages map[string]*state.Outage `json:"outages,omitempty"`
	// Unavailable lists the services that are unavailable now, also
	// without an outage, see state.Availability.
	Unavailable []string `json:"unavailable,omitempty"`
	// Services lists the services that the bot uses, for the
	// availability page.
	Services []string `json:"services"`
	// StaleAfter is the age in seconds after which the data is stale.
	StaleAfter int64   `json:"stale_after,omitempty"`
	Mode       Mode    `json:"mode"`
	Queue      int     `json:"queue"`
	Months     []Month `json:"months"`
}

type Mode struct {
	Push   string `json:"push"`
	Checks string `json:"checks"`
	Email  string `json:"email"`
}

type seriesFile struct {
	Schema int             `json:"schema"`
	Series []*state.Series `json:"series"`
}

// Run is the bot run that renders the pages.
type Run struct {
	URL string
	// Failed is set when the bot failed before the pages were rendered;
	// the pages are rendered anyway, from the last saved state.
	Failed bool
}

func Render(st *state.Store, cfg *config.Config, out string, now time.Time, run Run) error {
	if err := copySite(out); err != nil {
		return err
	}
	data := filepath.Join(out, "data")
	idx := Index{
		Schema:           state.SchemaVersion,
		Title:            cfg.Pages.Title,
		Name:             cfg.Name,
		Generated:        state.FormatTime(now),
		RunURL:           run.URL,
		RunFailed:        run.Failed,
		PatchworkWeb:     pw.UpgradeHTTP(cfg.PatchworkWebURL()),
		Project:          cfg.Patchwork.Project,
		TestRepo:         cfg.TestRepo.Repo,
		GitHost:          cfg.TestRepo.GitHost,
		ArchiveURLFormat: cfg.Pages.ArchiveURLFormat,
		PatchURLFormat:   cfg.Pages.PatchArchiveURLFormat,
		ListTag:          cfg.Pages.ListTag,
		Outages:          st.Meta.Outages,
		StaleAfter:       int64(cfg.Pages.StaleAfter.D().Seconds()),
		Mode: Mode{Push: cfg.Mode.Push, Checks: cfg.Mode.Checks,
			Email: cfg.Mode.Email},
		Queue:  len(st.Queue),
		Months: []Month{},
	}
	if err := writeJSON(filepath.Join(data, "availability.json"),
		st.Availability); err != nil {
		return err
	}
	idx.Services = services(cfg)
	for service := range st.Availability.Failing {
		idx.Unavailable = append(idx.Unavailable, service)
	}
	sort.Strings(idx.Unavailable)
	queue := upgradeLinks(st.Queue)
	if err := writeJSON(filepath.Join(data, "queue.json"),
		seriesFile{Schema: state.SchemaVersion, Series: queue}); err != nil {
		return err
	}
	months, err := st.HistoryMonths()
	if err != nil {
		return err
	}
	for _, m := range months {
		list, err := st.History(m)
		if err != nil {
			return err
		}
		n := 0
		for _, s := range list {
			n += len(s.Patches)
		}
		idx.Months = append(idx.Months, Month{Month: m, Series: len(list), Patches: n})
		file := seriesFile{Schema: state.SchemaVersion, Series: upgradeLinks(list)}
		if err := writeJSON(filepath.Join(data, "history", m+".json"), file); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(data, "index.json"), idx)
}

// services lists the services that the bot uses with this
// configuration, as named in state.Availability.
func services(cfg *config.Config) []string {
	list := []string{state.ServicePatchwork}
	if cfg.Mode.PostChecks() {
		list = append(list, state.ServicePatchworkChecks)
	}
	list = append(list, state.ServiceGitHub, state.ServiceWorkflows)
	if !cfg.Mode.EmailOff() {
		list = append(list, state.ServiceEmail)
	}
	if cfg.Archive.Ingest && cfg.Archive.ReportsURL != "" {
		list = append(list, state.OutageArchive("reports"))
	}
	if cfg.Rechecks.Enabled && !cfg.UseCommentEvents() && cfg.Archive.DevURL != "" {
		list = append(list, state.OutageArchive("dev"))
	}
	return list
}

// upgradeLinks returns the series with https:// Patchwork links, copying
// the changed ones so that the state itself is left alone.
func upgradeLinks(list []*state.Series) []*state.Series {
	out := make([]*state.Series, 0, len(list))
	for _, s := range list {
		if u := pw.UpgradeHTTP(s.WebURL); u != s.WebURL {
			c := *s
			c.WebURL = u
			s = &c
		}
		out = append(out, s)
	}
	return out
}

func writeJSON(path string, v any) error {
	b, err := state.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// assets are the files that index.html loads.  Their links carry a hash
// of their contents, so that browsers that cached an older version, for
// up to 10 minutes on GitHub Pages, do not use it with a newer page.
var assets = []string{"style.css", "app.js"}

func copySite(out string) error {
	page, err := site.ReadFile("site/index.html")
	if err != nil {
		return err
	}
	for _, name := range assets {
		b, err := site.ReadFile("site/" + name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		v := name + "?v=" + hex.EncodeToString(sum[:4])
		page = bytes.ReplaceAll(page, []byte(`"`+name+`"`), []byte(`"`+v+`"`))
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "index.html"), page, 0o644); err != nil {
		return err
	}
	return fs.WalkDir(site, "site", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("site", path)
		dst := filepath.Join(out, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if rel == "index.html" {
			return nil
		}
		b, err := site.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}
