// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

// Package pages renders the GitHub Pages site.  The site is static and
// reads the same JSON files as the bot, plus a small index.
package pages

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"leadtestkit/internal/config"
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
	Schema           int     `json:"schema"`
	Title            string  `json:"title"`
	Name             string  `json:"name"`
	Generated        string  `json:"generated"`
	RunURL           string  `json:"run_url,omitempty"`
	PatchworkWeb     string  `json:"patchwork_web"`
	Project          string  `json:"project"`
	TestRepo         string  `json:"test_repo"`
	GitHost          string  `json:"git_host"`
	ArchiveURLFormat string  `json:"archive_url_format,omitempty"`
	PatchURLFormat   string  `json:"patch_archive_url_format,omitempty"`
	Mode             Mode    `json:"mode"`
	Queue            int     `json:"queue"`
	Months           []Month `json:"months"`
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

func Render(st *state.Store, cfg *config.Config, out string, now time.Time, runURL string) error {
	if err := copySite(out); err != nil {
		return err
	}
	data := filepath.Join(out, "data")
	idx := Index{
		Schema:           state.SchemaVersion,
		Title:            cfg.Pages.Title,
		Name:             cfg.Name,
		Generated:        state.FormatTime(now),
		RunURL:           runURL,
		PatchworkWeb:     cfg.PatchworkWebURL(),
		Project:          cfg.Patchwork.Project,
		TestRepo:         cfg.TestRepo.Repo,
		GitHost:          cfg.TestRepo.GitHost,
		ArchiveURLFormat: cfg.Pages.ArchiveURLFormat,
		PatchURLFormat:   cfg.Pages.PatchArchiveURLFormat,
		Mode: Mode{Push: cfg.Mode.Push, Checks: cfg.Mode.Checks,
			Email: cfg.Mode.Email},
		Queue:  len(st.Queue),
		Months: []Month{},
	}
	queue := st.Queue
	if queue == nil {
		queue = []*state.Series{}
	}
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
		if err := writeJSON(filepath.Join(data, "history", m+".json"),
			seriesFile{Schema: state.SchemaVersion, Series: list}); err != nil {
			return err
		}
	}
	return writeJSON(filepath.Join(data, "index.json"), idx)
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

func copySite(out string) error {
	return fs.WalkDir(site, "site", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("site", path)
		dst := filepath.Join(out, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := site.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}
