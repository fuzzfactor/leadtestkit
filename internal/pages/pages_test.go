// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"leadtestkit/internal/config"
	"leadtestkit/internal/state"
)

const cfgYAML = `
patchwork:
  api_url: https://pw.example.org/api/1.2
  project: lorem
upstream:
  git_url: https://git.example.org/lorem.git
test_repo:
  repo: ipsum/lorem
mode:
  email: "off"
rechecks:
  enabled: false
pages:
  title: Lorem CI
`

func TestRender(t *testing.T) {
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, _ := state.Load(dir)
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	done := &state.Series{ID: 1, Status: state.SeriesDone, Discovered: state.FormatTime(now),
		Patches: []*state.Patch{{ID: 11, Status: state.PatchDone}}}
	botURL := "https://github.com/x/y/actions/runs/1/attempts/2"
	done.Decisions = []state.Decision{{Time: state.FormatTime(now),
		Action: "discovered", Detail: "Found in Patchwork.", RunURL: botURL}}
	done.Patches[0].Decisions = []state.Decision{{Time: state.FormatTime(now),
		Action: "pushed", Detail: "Updated the test branch.", RunURL: botURL}}
	st.Add(done)
	done.Finish(state.SeriesDone, "", now)
	if err := st.Archive(done); err != nil {
		t.Fatal(err)
	}
	st.Add(&state.Series{ID: 2, Status: state.SeriesQueued, Patches: []*state.Patch{}})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "site")
	if err := Render(st, cfg, out, now, "https://github.com/x/y/actions/runs/1"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"index.html", "app.js", "style.css", "data/queue.json",
		"data/history/2026-03.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Error(err)
		}
	}
	var idx Index
	b, _ := os.ReadFile(filepath.Join(out, "data", "index.json"))
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.Title != "Lorem CI" || idx.Queue != 1 || len(idx.Months) != 1 ||
		idx.Months[0].Patches != 1 ||
		idx.PatchworkWeb != "https://pw.example.org" {
		t.Errorf("index = %+v", idx)
	}
	b, err = os.ReadFile(filepath.Join(out, "data", "history", "2026-03.json"))
	if err != nil {
		t.Fatal(err)
	}
	var history seriesFile
	if err := json.Unmarshal(b, &history); err != nil {
		t.Fatal(err)
	}
	s := history.Series[0]
	if s.Decisions[0].RunURL != botURL || s.Patches[0].Decisions[0].RunURL != botURL {
		t.Fatalf("decision links were lost in Pages data: %+v", s)
	}
}
