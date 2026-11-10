// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package pages

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
  list_tag: lorem-dev
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
	st.Meta.Outages = map[string]*state.Outage{state.OutageRuns: {
		Since: state.FormatTime(now), Reason: "no workflow run started"}}
	st.Availability.Unavailable(state.ServicePatchwork, "lorem", now)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "site")
	if err := Render(st, cfg, out, now, Run{URL: "https://github.com/x/y/actions/runs/1",
		Failed: true}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"index.html", "app.js", "style.css", "data/queue.json",
		"data/history/2026-03.json", "data/availability.json"} {
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
		idx.Months[0].Patches != 1 || idx.ListTag != "lorem-dev" ||
		idx.Outages[state.OutageRuns].Reason != "no workflow run started" ||
		idx.StaleAfter != 7200 || !idx.RunFailed ||
		fmt.Sprint(idx.Unavailable) != "[patchwork]" ||
		idx.PatchworkWeb != "https://pw.example.org" {
		t.Errorf("index = %+v", idx)
	}
	var avail state.Availability
	b, _ = os.ReadFile(filepath.Join(out, "data", "availability.json"))
	if err := json.Unmarshal(b, &avail); err != nil ||
		avail.Failing[state.ServicePatchwork].Reason != "lorem" {
		t.Errorf("availability = %s, %v", b, err)
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

func TestRenderUpgradesPatchworkLinks(t *testing.T) {
	cfg, err := config.Parse([]byte(strings.Replace(cfgYAML,
		"https://pw.example.org", "http://pw.example.org", 1)))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := state.Load(t.TempDir())
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	series := func(id int) *state.Series {
		return &state.Series{ID: id, Status: state.SeriesQueued,
			Patches: []*state.Patch{},
			WebURL: fmt.Sprintf("http://pw.example.org/project/lorem/list/?series=%d",
				id)}
	}
	done := series(1)
	st.Add(done)
	done.Finish(state.SeriesDone, "", now)
	if err := st.Archive(done); err != nil {
		t.Fatal(err)
	}
	queued := series(2)
	st.Add(queued)
	out := t.TempDir()
	if err := Render(st, cfg, out, now, Run{}); err != nil {
		t.Fatal(err)
	}
	var idx Index
	b, _ := os.ReadFile(filepath.Join(out, "data", "index.json"))
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.PatchworkWeb != "https://pw.example.org" {
		t.Errorf("patchwork web = %q", idx.PatchworkWeb)
	}
	for _, f := range []string{"queue.json", "history/2026-03.json"} {
		var data seriesFile
		b, _ := os.ReadFile(filepath.Join(out, "data", f))
		if err := json.Unmarshal(b, &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Series) != 1 ||
			!strings.HasPrefix(data.Series[0].WebURL, "https://pw.example.org/") {
			t.Errorf("%s: series links = %+v", f, data.Series)
		}
	}
	if !strings.HasPrefix(queued.WebURL, "http://") ||
		!strings.HasPrefix(done.WebURL, "http://") {
		t.Error("rendering changed the saved state")
	}
}
