// Copyright 2026 LeadTestKit Authors
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"leadtestkit/internal/gitrepo"
)

var now = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func sampleSeries(id int) *Series {
	return &Series{
		ID:         id,
		Name:       "[v2,0/2] lorem: Ipsum",
		Date:       "2026-03-10T10:00:00",
		Submitter:  Person{Name: "Lorem Ipsum", Email: "lorem@example.org"},
		Total:      2,
		Status:     SeriesQueued,
		Discovered: FormatTime(now),
		Patches: []*Patch{
			{ID: id*10 + 1, Number: 1, Name: "[v2,1/2] lorem", MsgID: "<1@x>", Status: PatchPending},
			{ID: id*10 + 2, Number: 2, Name: "[v2,2/2] ipsum", MsgID: "<2@x>", Status: PatchPending},
		},
	}
}

func TestLoadEmpty(t *testing.T) {
	s, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.Meta.Schema != SchemaVersion || len(s.Queue) != 0 {
		t.Errorf("store = %+v", s)
	}
}

func TestSaveLoadRoundTripAndStableOutput(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	s.Add(sampleSeries(20))
	s.Add(sampleSeries(10))
	s.Meta.Events["series-completed"] = 77
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(filepath.Join(dir, QueueFile))

	s2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Queue) != 2 || s2.Queue[0].ID != 10 || s2.Meta.Events["series-completed"] != 77 {
		t.Fatalf("loaded = %+v", s2.Queue)
	}
	if err := s2.Save(); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(filepath.Join(dir, QueueFile))
	if string(first) != string(second) {
		t.Error("save is not stable")
	}
	if !strings.Contains(string(first), "\n  \"series\": [\n    {\n      \"id\": 10,") {
		t.Errorf("unexpected format:\n%s", first)
	}
}

func TestLoadRejectsBadState(t *testing.T) {
	tests := map[string]string{
		"unknown field": `{"series":[{"id":1,"status":"queued","lorem":1,"patches":[]}]}`,
		"bad status":    `{"series":[{"id":1,"status":"lorem","patches":[]}]}`,
		"duplicate":     `{"series":[{"id":1,"status":"new","patches":[]},{"id":1,"status":"new","patches":[]}]}`,
		"bad json":      `{"series":[`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, QueueFile), []byte(content), 0o644)
			if _, err := Load(dir); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestArchiveAndReopen(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	x := sampleSeries(5)
	s.Add(x)
	x.Finish(SeriesDone, "", now)
	if err := s.Archive(x); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if len(s.Queue) != 0 {
		t.Fatal("series still queued")
	}
	if _, err := os.Stat(filepath.Join(dir, HistoryDir, "2026-03.json")); err != nil {
		t.Fatal(err)
	}

	s2, _ := Load(dir)
	months, _ := s2.HistoryMonths()
	if len(months) != 1 || months[0] != "2026-03" {
		t.Fatalf("months = %v", months)
	}
	recent, err := s2.Recent(now, 24*time.Hour)
	if err != nil || len(recent) != 1 {
		t.Fatalf("recent = %v err = %v", recent, err)
	}
	if _, p := FindPatch(recent, 52); p == nil {
		t.Error("patch not found")
	}
	if err := s2.Reopen(recent[0]); err != nil {
		t.Fatal(err)
	}
	s2.Save()
	h, _ := s2.History("2026-03")
	if len(s2.Queue) != 1 || len(h) != 0 {
		t.Errorf("queue = %d history = %d", len(s2.Queue), len(h))
	}
	if recent, _ := s2.Recent(now.Add(60*24*time.Hour), 24*time.Hour); len(recent) != 1 {
		t.Errorf("recent = %d", len(recent))
	}
}

func TestRecordFailure(t *testing.T) {
	var f *Failure
	if !f.Due(now) {
		t.Error("nil failure must be due")
	}
	if RecordFailure(&f, "apply", errors.New("lorem"), now, time.Hour) {
		t.Error("deadline passed too early")
	}
	if f.Attempts != 1 || f.Due(now) || !f.Due(now.Add(5*time.Minute)) {
		t.Errorf("failure = %+v", f)
	}
	RecordFailure(&f, "apply", errors.New("ipsum"), now.Add(10*time.Minute), time.Hour)
	if f.Attempts != 2 || f.LastError != "ipsum" || f.First != FormatTime(now) {
		t.Errorf("failure = %+v", f)
	}
	if !RecordFailure(&f, "apply", errors.New("x"), now.Add(2*time.Hour), time.Hour) {
		t.Error("deadline must have passed")
	}
	RecordFailure(&f, "push", errors.New("y"), now, time.Hour)
	if f.Op != "push" || f.Attempts != 1 {
		t.Errorf("new op must reset: %+v", f)
	}
}

func gitInit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=x", "GIT_AUTHOR_EMAIL=x@example.org",
		"GIT_COMMITTER_NAME=x", "GIT_COMMITTER_EMAIL=x@example.org", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func setupRepos(t *testing.T) (remote, a, b string) {
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	a = filepath.Join(root, "a")
	b = filepath.Join(root, "b")
	gitInit(t, root, "init", "-q", "--bare", "-b", "state", remote)
	gitInit(t, root, "clone", "-q", remote, a)
	os.WriteFile(filepath.Join(a, "config.yaml"), []byte("enabled: true\n"), 0o644)
	gitInit(t, a, "add", ".")
	gitInit(t, a, "commit", "-qm", "config")
	gitInit(t, a, "push", "-q", "origin", "HEAD:refs/heads/state")
	gitInit(t, root, "clone", "-q", "-b", "state", remote, b)
	return
}

func testRepo(dir string) *Repo {
	return &Repo{
		Git:    &gitrepo.Git{Dir: dir, Env: []string{"GIT_CONFIG_GLOBAL=/dev/null"}},
		Remote: "origin",
		Branch: "state",
		Author: "Lorem Bot <bot@example.org>",
	}
}

func TestRepoCommitPush(t *testing.T) {
	_, a, _ := setupRepos(t)
	ctx := context.Background()
	r := testRepo(a)
	if ok, err := r.Commit(ctx, "nothing"); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	s, _ := Load(a)
	s.Add(sampleSeries(1))
	s.Save()
	if ok, err := r.Commit(ctx, "add series 1"); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if rebased, err := r.Push(ctx); rebased || err != nil {
		t.Fatalf("rebased=%v err=%v", rebased, err)
	}
	author, _ := r.Git.Out(ctx, "log", "-1", "--format=%an <%ae>")
	if author != "Lorem Bot <bot@example.org>" {
		t.Errorf("author = %q", author)
	}
}

func TestRepoPushRebasesOverHumanEdit(t *testing.T) {
	_, a, b := setupRepos(t)
	ctx := context.Background()

	// A human holds series 1 in clone b.
	sb, _ := Load(b)
	x := sampleSeries(1)
	sb.Add(x)
	sb.Save()
	rb := testRepo(b)
	rb.Commit(ctx, "series 1")
	rb.Push(ctx)

	ra := testRepo(a)
	gitInit(t, a, "pull", "-q", "origin", "state")
	sa, _ := Load(a)
	sa.Find(1).Hold = true
	sa.Save()
	ra.Commit(ctx, "hold 1")
	ra.Push(ctx)

	// The bot, working in b without the human change, updates patches.
	sb.Find(1).Patches[0].Status = PatchApplied
	sb.Add(sampleSeries(2))
	sb.Save()
	rb.Commit(ctx, "bot update")
	rebased, err := rb.Push(ctx)
	if err != nil || !rebased {
		t.Fatalf("rebased=%v err=%v", rebased, err)
	}
	merged, err := Load(b)
	if err != nil {
		t.Fatal(err)
	}
	if !merged.Find(1).Hold || merged.Find(1).Patches[0].Status != PatchApplied || merged.Find(2) == nil {
		t.Errorf("merge lost changes: %+v", merged.Find(1))
	}
}
